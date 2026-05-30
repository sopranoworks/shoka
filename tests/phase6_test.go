package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/shoka/mcp-server/internal/webhooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newConcreteStorage(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-phase6-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := storage.NewFSGitStorage(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.CreateProject("ns", "proj"))
	return s
}

func wireWebhook(s *storage.FSGitStorage, n *webhooks.Notifier) {
	s.SetChangeHandler(func(ev storage.ChangeEvent) {
		n.Emit(webhooks.Event{
			Event:      ev.Event,
			Namespace:  ev.Namespace,
			Project:    ev.Project,
			Path:       ev.Path,
			CommitHash: ev.CommitHash,
			Timestamp:  ev.Timestamp,
		})
	})
}

func TestPhase6_WriteEmitsWebhook(t *testing.T) {
	s := newConcreteStorage(t)
	ctx := context.Background()

	got := make(chan webhooks.Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev webhooks.Event
		_ = json.Unmarshal(b, &ev)
		got <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := webhooks.New([]webhooks.Config{{URL: srv.URL, Events: []string{"file_written"}}})
	wireWebhook(s, n)

	write := tools.WriteFileHandler(s)
	res, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "hi"})
	require.NoError(t, err)
	require.Nil(t, res)

	drainTool(t, s) // the webhook fires from the background commit (async)
	n.Wait()
	select {
	case ev := <-got:
		assert.Equal(t, "file_written", ev.Event)
		assert.Equal(t, "ns", ev.Namespace)
		assert.Equal(t, "proj", ev.Project)
		assert.Equal(t, "a.md", ev.Path)
		assert.NotEmpty(t, ev.CommitHash)
	default:
		t.Fatal("expected a webhook delivery for the write")
	}
}

func TestPhase6_WebhookFailureDoesNotFailWrite(t *testing.T) {
	s := newConcreteStorage(t)
	ctx := context.Background()

	n := webhooks.New([]webhooks.Config{{URL: "http://127.0.0.1:1/dead", Events: []string{"file_written"}}})
	wireWebhook(s, n)

	write := tools.WriteFileHandler(s)
	res, out, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "hi"})
	require.NoError(t, err)
	require.Nil(t, res)
	assert.NotEmpty(t, out.ETag, "the write must succeed even when the webhook delivery fails")

	drainTool(t, s)
	n.Wait() // must not hang despite the dead endpoint
}

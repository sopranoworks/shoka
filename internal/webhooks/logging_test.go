package webhooks

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotifier_LogsDeliveryOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New([]Config{{Name: "h1", URL: srv.URL, Events: []string{"file_written"}}})
	var buf bytes.Buffer
	n.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	n.Emit(Event{Event: "file_written", Namespace: "ns", Project: "p", Path: "a.md", Timestamp: time.Now()})
	n.Wait()

	out := buf.String()
	if !strings.Contains(out, "webhook delivered") || !strings.Contains(out, "h1") {
		t.Errorf("expected delivery success log: %q", out)
	}
}

func TestNotifier_NilLoggerSafe(t *testing.T) {
	n := New([]Config{{Name: "h1", URL: "http://127.0.0.1:0", Events: []string{"file_written"}}})
	n.Emit(Event{Event: "file_written", Timestamp: time.Now()}) // must not panic with nil logger
	n.Wait()
}

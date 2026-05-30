package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoka/mcp-server/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminAPI_StatusRescanRecover(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFSGitStorage(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err = s.Write(context.Background(), "", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	require.True(t, s.WaitForWAL(10*1e9))

	srv := httptest.NewServer(New(s))
	defer srv.Close()

	get := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}

	// status: healthy
	code, m := get(http.MethodGet, "/api/project/status?namespace=ns&project=proj")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "healthy", m["state"])

	// hand-edit the working tree, then rescan -> corrupted
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "proj", "a.md"), []byte("hand-edited"), 0o644))
	code, m = get(http.MethodPost, "/api/project/rescan?namespace=ns&project=proj")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "corrupted", m["state"])
	assert.Contains(t, m["modified"], "a.md")

	// recover (accept-working-tree) -> healthy
	code, m = get(http.MethodPost, "/api/project/recover?namespace=ns&project=proj&mode=accept-working-tree")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "healthy", m["state"])

	// bad mode -> 400
	code, _ = get(http.MethodPost, "/api/project/recover?namespace=ns&project=proj&mode=bogus")
	assert.Equal(t, http.StatusBadRequest, code)

	// missing project -> 400
	code, _ = get(http.MethodGet, "/api/project/status?namespace=ns")
	assert.Equal(t, http.StatusBadRequest, code)
}

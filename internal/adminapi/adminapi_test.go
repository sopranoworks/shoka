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

	"github.com/sopranoworks/shoka/internal/storage"
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

	srv := httptest.NewServer(New(s, storage.SnapshotSweepConfig{}))
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

// TestAdminAPI_Snapshot drives POST /api/snapshot: it runs a real cycle on the
// running server (no second storage instance) and returns the written/pruned
// summary; an unconfigured output_dir is a clean 400.
func TestAdminAPI_Snapshot(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.NewFSGitStorage(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err = s.Write(context.Background(), "", "ns", "proj", "a.md", "hello\n", nil)
	require.NoError(t, err)
	require.True(t, s.WaitForWAL(10*1e9))

	out := t.TempDir()
	backup := storage.SnapshotSweepConfig{OutputDir: out, Scope: storage.Scope{}, RetentionCount: 7}
	srv := httptest.NewServer(New(s, backup))
	defer srv.Close()

	post := func(path string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}

	// A configured cycle writes one archive for the seeded project.
	code, m := post("/api/snapshot")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, float64(1), m["written"], "one project snapshotted")
	if _, statErr := os.Stat(filepath.Join(out, "ns", "proj")); statErr != nil {
		t.Fatalf("expected an archive dir for ns/proj: %v", statErr)
	}

	// An unconfigured output_dir is a clean 400 (not a panic / 500).
	srvNoDir := httptest.NewServer(New(s, storage.SnapshotSweepConfig{}))
	defer srvNoDir.Close()
	req, _ := http.NewRequest(http.MethodPost, srvNoDir.URL+"/api/snapshot", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

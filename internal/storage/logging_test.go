package storage

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestStorage_LogsCommitOnWrite_NotContent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSGitStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var buf bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create: %v", err)
	}
	const secret = "COMMIT-SECRET-CONTENT-7b21"
	if _, err := s.WriteFileVersioned("ns", "proj", "a.md", secret, ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}
	// The commit hash is now a git hash assigned by the background worker; fetch it.
	hist, err := s.GetHistory("ns", "proj", "a.md", 1)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history: %v (%d)", err, len(hist))
	}
	hash := hist[0].Hash

	out := buf.String()
	if !strings.Contains(out, "git change committed") {
		t.Errorf("missing commit log: %q", out)
	}
	if !strings.Contains(out, hash) || !strings.Contains(out, "a.md") {
		t.Errorf("commit log missing hash/path: %q", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("file content leaked into storage logs: %q", out)
	}
}

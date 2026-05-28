package storage

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestStorage_LogsCommitOnWrite_NotContent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSGitStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	var buf bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create: %v", err)
	}
	const secret = "COMMIT-SECRET-CONTENT-7b21"
	hash, err := s.WriteFileVersioned("ns", "proj", "a.md", secret, "")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
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

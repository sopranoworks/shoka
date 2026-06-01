package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/storage/wal"
)

func newIdentityStorage(t *testing.T) *FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-identity-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewFSGitStorageWithOptions(dir, Options{
		Identity: identity.Defaults{
			UserName:  "Osamu Takahashi",
			UserEmail: "forte.nit@gmail.com",
			AgentName: "shoka-agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s
}

func headCommit(t *testing.T, s *FSGitStorage) *object.Commit {
	t.Helper()
	r, err := git.PlainOpen(filepath.Join(s.baseDir, "ns", "proj"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCommitIdentity_DefaultAgent(t *testing.T) {
	s := newIdentityStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "a.md", "# A", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	c := headCommit(t, s)
	if c.Author.Name != "shoka-agent" {
		t.Errorf("author name = %q, want shoka-agent", c.Author.Name)
	}
	if c.Author.Email != "shoka-agent@agents.shoka.local" {
		t.Errorf("author email = %q", c.Author.Email)
	}
	if c.Committer.Name != "Osamu Takahashi" || c.Committer.Email != "forte.nit@gmail.com" {
		t.Errorf("committer = %s <%s>, want the configured user", c.Committer.Name, c.Committer.Email)
	}
	if !strings.Contains(c.Message, "Shoka-User: Osamu Takahashi <forte.nit@gmail.com>") {
		t.Errorf("missing user trailer:\n%s", c.Message)
	}
	if !strings.Contains(c.Message, "Shoka-Agent: shoka-agent") {
		t.Errorf("missing agent trailer:\n%s", c.Message)
	}
	if strings.Contains(c.Message, "Shoka-Worker") {
		t.Errorf("unexpected worker trailer:\n%s", c.Message)
	}
}

func TestCommitIdentity_DeclaredAgentAndWorker(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := identity.WithAgent(context.Background(), identity.Agent{Name: "claude-code", Worker: "w-42"})
	if _, err := s.Write(ctx, "", "ns", "proj", "b.md", "# B", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	c := headCommit(t, s)
	if c.Author.Name != "claude-code" {
		t.Errorf("author = %q, want claude-code", c.Author.Name)
	}
	if c.Committer.Name != "Osamu Takahashi" {
		t.Errorf("committer = %q, want the user", c.Committer.Name)
	}
	if !strings.Contains(c.Message, "Shoka-Agent: claude-code") {
		t.Errorf("missing agent trailer:\n%s", c.Message)
	}
	if !strings.Contains(c.Message, "Shoka-Worker: w-42") {
		t.Errorf("missing worker trailer:\n%s", c.Message)
	}
}

// A web /ws/ui SAVE_FILE (WithUser) makes the owning user the git Author, while
// the committer stays the user and the Shoka-Agent trailer still records the
// (default) agent layer. Contrast with TestCommitIdentity_DefaultAgent, where the
// same write without WithUser is agent-authored.
func TestCommitIdentity_WithUserMakesUserAuthor(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := identity.WithUser(context.Background(), identity.User{})
	if _, err := s.Write(ctx, "", "ns", "proj", "web.md", "# Web", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	c := headCommit(t, s)
	if c.Author.Name != "Osamu Takahashi" || c.Author.Email != "forte.nit@gmail.com" {
		t.Errorf("author = %s <%s>, want the operator user", c.Author.Name, c.Author.Email)
	}
	if c.Committer.Name != "Osamu Takahashi" || c.Committer.Email != "forte.nit@gmail.com" {
		t.Errorf("committer = %s <%s>, want the operator user", c.Committer.Name, c.Committer.Email)
	}
	if !strings.Contains(c.Message, "Shoka-User: Osamu Takahashi <forte.nit@gmail.com>") {
		t.Errorf("missing user trailer:\n%s", c.Message)
	}
	if !strings.Contains(c.Message, "Shoka-Agent: shoka-agent") {
		t.Errorf("agent trailer should still record the agent layer:\n%s", c.Message)
	}
}

// An entry written before identity fields existed (empty identity) still gets an
// intentional author from the configured default — not a zero/environmental one.
func TestCommitIdentity_OlderEntryFallsBackToDefault(t *testing.T) {
	s := newIdentityStorage(t)
	if _, err := s.wal.Append(wal.Entry{
		Namespace: "ns",
		Project:   "proj",
		Path:      "old.md",
		Op:        "write",
		Content:   []byte("# Old"),
		// no identity fields — simulates a pre-upgrade WAL entry
	}); err != nil {
		t.Fatal(err)
	}
	s.pool.Notify()
	drain(t, s)

	c := headCommit(t, s)
	if c.Author.Name != "shoka-agent" {
		t.Errorf("author = %q, want default shoka-agent", c.Author.Name)
	}
	if c.Committer.Name != "Osamu Takahashi" {
		t.Errorf("committer = %q, want configured user", c.Committer.Name)
	}
}

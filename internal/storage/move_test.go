package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/notify"
)

// mustWrite writes a file through the unchecked write path and fails on error.
func mustWrite(t *testing.T, s *FSGitStorage, path, content string) {
	t.Helper()
	if _, err := s.Write(context.Background(), "", "ns", "proj", path, content, nil); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newStorageWithCenter builds storage wired to a notify center, with ns/proj.
func newStorageWithCenter(t *testing.T, center *notify.Center) *FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-move-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewFSGitStorageWithOptions(dir, Options{NotifyCenter: center})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s
}

func headHashFor(t *testing.T, s *FSGitStorage, ns, proj string) plumbing.Hash {
	t.Helper()
	r, err := git.PlainOpen(s.baseDir + "/" + ns + "/" + proj)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	return ref.Hash()
}

func blobHashAt(t *testing.T, s *FSGitStorage, ns, proj, commitRef, path string) plumbing.Hash {
	t.Helper()
	r, err := git.PlainOpen(s.baseDir + "/" + ns + "/" + proj)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(plumbing.NewHash(commitRef))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := tree.File(path)
	if err != nil {
		return plumbing.ZeroHash
	}
	return f.Hash
}

func TestMove_BasicRenamePreservesBlobAndHistory(t *testing.T) {
	s := newTestStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "a.md", "# A\n", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	preHead := headHashFor(t, s, "ns", "proj")
	srcBlob := blobHashAt(t, s, "ns", "proj", preHead.String(), "a.md")

	etag, links, err := s.Move(context.Background(), "", "ns", "proj", "a.md", "b.md", nil)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if links != 0 {
		t.Errorf("links rewritten = %d, want 0", links)
	}
	if etag != sha256Hex([]byte("# A\n")) {
		t.Errorf("etag = %q, want sha256 of moved content", etag)
	}
	drain(t, s)

	// Working tree: source gone, target present with identical content.
	if _, err := s.ReadFile("ns", "proj", "a.md"); err == nil {
		t.Error("source a.md should be gone")
	}
	got, err := s.ReadFile("ns", "proj", "b.md")
	if err != nil || got != "# A\n" {
		t.Errorf("target b.md = %q, err=%v; want %q", got, err, "# A\n")
	}

	// Atomic single commit: HEAD advanced by exactly one commit, parent == preHead.
	r, _ := git.PlainOpen(s.baseDir + "/ns/proj")
	headRef, _ := r.Head()
	headCommit, _ := r.CommitObject(headRef.Hash())
	if len(headCommit.ParentHashes) != 1 || headCommit.ParentHashes[0] != preHead {
		t.Errorf("move did not produce exactly one commit on top of preHead (parents=%v)", headCommit.ParentHashes)
	}
	if !strings.HasPrefix(headCommit.Message, "Move a.md -> b.md") {
		t.Errorf("commit subject = %q, want 'Move a.md -> b.md'", headCommit.Message)
	}

	// History preservation: the destination blob hash equals the source's
	// last-committed blob hash — exactly what `git log --follow` keys on.
	dstBlob := blobHashAt(t, s, "ns", "proj", headRef.Hash().String(), "b.md")
	if dstBlob != srcBlob || dstBlob == plumbing.ZeroHash {
		t.Errorf("destination blob %s != source blob %s (rename detection would fail)", dstBlob, srcBlob)
	}
}

func TestMove_ConflictOnSourceWhenTargetAbsent(t *testing.T) {
	s := newTestStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "a.md", "AAA", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	stale := "deadbeef"
	_, _, err := s.Move(context.Background(), "", "ns", "proj", "a.md", "b.md", &stale)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want VersionConflictError, got %v", err)
	}
	if conflict.Current != sha256Hex([]byte("AAA")) {
		t.Errorf("conflict current = %q, want source etag", conflict.Current)
	}
	// Nothing moved.
	if _, err := s.ReadFile("ns", "proj", "a.md"); err != nil {
		t.Error("source must still exist after a conflict")
	}
	if _, err := s.ReadFile("ns", "proj", "b.md"); err == nil {
		t.Error("target must not exist after a conflict")
	}
}

func TestMove_TargetExistsNoIfMatchRefused(t *testing.T) {
	s := newTestStorage(t)
	mustWrite(t, s, "a.md", "AAA")
	mustWrite(t, s, "b.md", "BBB")
	drain(t, s)
	_, _, err := s.Move(context.Background(), "", "ns", "proj", "a.md", "b.md", nil)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want VersionConflictError (no silent overwrite), got %v", err)
	}
	if conflict.Current != sha256Hex([]byte("BBB")) {
		t.Errorf("conflict current = %q, want target etag", conflict.Current)
	}
	got, _ := s.ReadFile("ns", "proj", "b.md")
	if got != "BBB" {
		t.Errorf("target overwritten despite refusal: %q", got)
	}
}

func TestMove_TargetExistsWithIfMatchOverwrites(t *testing.T) {
	s := newTestStorage(t)
	mustWrite(t, s, "a.md", "AAA")
	mustWrite(t, s, "b.md", "BBB")
	drain(t, s)
	bEtag := sha256Hex([]byte("BBB"))
	_, _, err := s.Move(context.Background(), "", "ns", "proj", "a.md", "b.md", &bEtag)
	if err != nil {
		t.Fatalf("overwrite move: %v", err)
	}
	drain(t, s)
	got, err := s.ReadFile("ns", "proj", "b.md")
	if err != nil || got != "AAA" {
		t.Errorf("target b.md = %q (err %v), want overwritten with AAA", got, err)
	}
	if _, err := s.ReadFile("ns", "proj", "a.md"); err == nil {
		t.Error("source a.md should be gone after overwrite move")
	}
}

func TestMove_SamePathRejected(t *testing.T) {
	s := newTestStorage(t)
	mustWrite(t, s, "a.md", "AAA")
	drain(t, s)
	if _, _, err := s.Move(context.Background(), "", "ns", "proj", "a.md", "a.md", nil); err == nil {
		t.Fatal("moving a file onto itself must error")
	}
}

// TestMove_DoesNotRewriteInboundLinks pins the path-only contract (backlog B-33,
// directives/2026-06-03-shoka-move-file-disable-link-rewrite): a move changes ONLY
// the moved file's path. Inbound referrers are left byte-for-byte untouched and
// links_rewritten is 0, while the rename is still a single atomic,
// history-preserving commit. (Converted from the former
// TestMove_RewritesInboundLinksInOneCommit, which asserted the now-disabled
// rewrite; the dormant rewriter's retained correctness is pinned separately by
// TestMove_DormantInboundLinkRewriteSeam and TestRewriteLinks.)
func TestMove_DoesNotRewriteInboundLinks(t *testing.T) {
	s := newTestStorage(t)
	mustWrite(t, s, "old.md", "# Old\n")
	mustWrite(t, s, "ref.md", "see [the doc](old.md) here\n")
	mustWrite(t, s, "docs/deep.md", "nested [link](../old.md)\n")
	mustWrite(t, s, "other.md", "unrelated [x](something.md)\n")
	drain(t, s)
	preHead := headHashFor(t, s, "ns", "proj")

	_, links, err := s.Move(context.Background(), "", "ns", "proj", "old.md", "new.md", nil)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if links != 0 {
		t.Fatalf("links rewritten = %d, want 0 (link auto-update on move is disabled)", links)
	}
	drain(t, s)

	// Referrers are UNTOUCHED — the inbound links still point at the old path.
	if got, _ := s.ReadFile("ns", "proj", "ref.md"); got != "see [the doc](old.md) here\n" {
		t.Errorf("ref.md = %q, want UNCHANGED (no link rewrite)", got)
	}
	if got, _ := s.ReadFile("ns", "proj", "docs/deep.md"); got != "nested [link](../old.md)\n" {
		t.Errorf("docs/deep.md = %q, want UNCHANGED", got)
	}
	if got, _ := s.ReadFile("ns", "proj", "other.md"); got != "unrelated [x](something.md)\n" {
		t.Errorf("other.md unexpectedly changed: %q", got)
	}

	// The rename is still ONE atomic commit on top of preHead, and only the path
	// changed: the old path is gone, the new path present.
	r, _ := git.PlainOpen(s.baseDir + "/ns/proj")
	headRef, _ := r.Head()
	hc, _ := r.CommitObject(headRef.Hash())
	if len(hc.ParentHashes) != 1 || hc.ParentHashes[0] != preHead {
		t.Errorf("move was not a single atomic commit (parents=%v)", hc.ParentHashes)
	}
	if blobHashAt(t, s, "ns", "proj", headRef.Hash().String(), "old.md") != plumbing.ZeroHash {
		t.Error("old.md still present in the committed tree")
	}
	// The referrer content is unchanged in that same commit's tree (not rewritten).
	if got := fileContentAt(t, r, headRef.Hash(), "ref.md"); got != "see [the doc](old.md) here\n" {
		t.Errorf("committed ref.md = %q, want unchanged", got)
	}
}

// TestMove_DormantInboundLinkRewriteSeam proves the retained-but-dormant inbound
// link-rewrite pipeline (discoverReferrers + the goldmark rewriter + Aux assembly)
// still works in isolation, even though storage.Move no longer calls it (the B-33
// re-enablement seam). This is the directive §2 non-negotiable: a retained direct
// unit test proving the rewriter still works, decoupled from the move path.
func TestMove_DormantInboundLinkRewriteSeam(t *testing.T) {
	s := newTestStorage(t)
	mustWrite(t, s, "old.md", "# Old\n")
	mustWrite(t, s, "ref.md", "see [the doc](old.md) here\n")
	mustWrite(t, s, "docs/deep.md", "nested [link](../old.md)\n")
	mustWrite(t, s, "other.md", "unrelated [x](something.md)\n")
	drain(t, s)

	projectPath := s.baseDir + "/ns/proj"
	aux, err := s.rewriteInboundLinksForMove(projectPath, "old.md", "new.md")
	if err != nil {
		t.Fatalf("dormant seam: %v", err)
	}
	// Two referrers link to old.md (ref.md, docs/deep.md); other.md does not.
	if len(aux) != 2 {
		t.Fatalf("aux len = %d, want 2 (dormant rewriter must still find both referrers)", len(aux))
	}
	got := map[string]string{}
	for _, a := range aux {
		got[a.Path] = string(a.Content)
	}
	if got["ref.md"] != "see [the doc](new.md) here\n" {
		t.Errorf("dormant rewrite of ref.md = %q, want pointed at new.md", got["ref.md"])
	}
	if got["docs/deep.md"] != "nested [link](../new.md)\n" {
		t.Errorf("dormant rewrite of docs/deep.md = %q, want relative link to new.md", got["docs/deep.md"])
	}
}

func TestMove_PublishesFileMoveEvent(t *testing.T) {
	center := notify.NewCenter(100)
	s := newStorageWithCenter(t, center)
	mustWrite(t, s, "old.md", "# Old\n")
	drain(t, s)

	if _, _, err := s.Move(context.Background(), "", "ns", "proj", "old.md", "new.md", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	var found *notify.Event
	for _, ev := range center.Snapshot() {
		ev := ev
		if ev.Kind == "file.move" {
			found = &ev
		}
	}
	if found == nil {
		t.Fatal("no file.move event published")
	}
	if found.SourcePath != "old.md" || found.Path != "new.md" {
		t.Errorf("event source=%q path=%q, want old.md/new.md", found.SourcePath, found.Path)
	}
	if found.Target != "ns/proj" {
		t.Errorf("event target = %q, want ns/proj", found.Target)
	}
}

// TestMove_WebIsUserAuthored pins the web-path guarantee: a move on a context
// carrying identity.WithUser is committed with the operator user as git Author
// (the /ws/ui MOVE_FILE attribution), the ui-layer half of which is asserted in
// ui.TestWSUI_MoveAttachesOperatorIdentity.
func TestMove_WebIsUserAuthored(t *testing.T) {
	s := newIdentityStorage(t) // configured user: Osamu Takahashi
	mustWrite(t, s, "a.md", "x")
	drain(t, s)
	ctx := identity.WithUser(context.Background(), identity.User{})
	if _, _, err := s.Move(ctx, "", "ns", "proj", "a.md", "b.md", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	drain(t, s)
	c := headCommit(t, s)
	if c.Author.Name != "Osamu Takahashi" {
		t.Errorf("author = %q, want the operator user (web move is user-authored)", c.Author.Name)
	}
	if c.Committer.Name != "Osamu Takahashi" {
		t.Errorf("committer = %q, want the operator user", c.Committer.Name)
	}
}

// TestMove_MCPIsAgentAuthored pins the MCP-path guarantee: a move on a context
// carrying identity.WithAgent is committed with the declaring agent as git Author.
func TestMove_MCPIsAgentAuthored(t *testing.T) {
	s := newIdentityStorage(t)
	mustWrite(t, s, "a.md", "x")
	drain(t, s)
	ctx := identity.WithAgent(context.Background(), identity.Agent{Name: "claude-code"})
	if _, _, err := s.Move(ctx, "", "ns", "proj", "a.md", "b.md", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	drain(t, s)
	c := headCommit(t, s)
	if c.Author.Name != "claude-code" {
		t.Errorf("author = %q, want claude-code (agent-as-author)", c.Author.Name)
	}
	if !strings.Contains(c.Message, "Shoka-Agent: claude-code") {
		t.Errorf("missing agent trailer:\n%s", c.Message)
	}
}

func fileContentAt(t *testing.T, r *git.Repository, h plumbing.Hash, path string) string {
	t.Helper()
	c, err := r.CommitObject(h)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := tree.File(path)
	if err != nil {
		return ""
	}
	content, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return content
}

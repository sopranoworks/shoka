package librarian

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixtureCorpus builds the small on-disk corpus the Stage-2 tests share:
//   - doc.md, guide/setup.md         — readable
//   - .git/config                    — ignored (default ".git/")
//   - .shoka.disposable              — ignored (injected ".shoka*")
//   - leak.txt -> <outside>/secret   — symlink leaf out of root (skipped)
//
// It returns the corpus root, the injected ignore patterns, and the secret
// string that must NEVER appear in any tool output.
func fixtureCorpus(t *testing.T) (root string, ignore []string, secret string) {
	t.Helper()
	root = t.TempDir()
	outside := t.TempDir()
	secret = "TOP-SECRET-DO-NOT-LEAK"

	writeFile(t, filepath.Join(root, "doc.md"), "# Doc\nThe capital fact is: the answer is 42.\n")
	mkdir(t, filepath.Join(root, "guide"))
	writeFile(t, filepath.Join(root, "guide", "setup.md"), "# Setup\nRun the server with --port.\n")
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, ".git", "config"), "[core]\n"+secret+"\n")
	writeFile(t, filepath.Join(root, ".shoka.disposable"), secret+"\n")
	writeFile(t, filepath.Join(outside, "secret.txt"), secret+"\n")

	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	return root, []string{".shoka*"}, secret
}

func readJSON(t *testing.T, args readArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestReadTool_Security proves the guard refuses every out-of-bounds read and
// never leaks the secret — the Stage-2 security guarantee, no LLM involved.
func TestReadTool_Security(t *testing.T) {
	root, ignore, secret := fixtureCorpus(t)
	g := NewGuard(root, ignore)
	read := readDispatch(g, NewDirCorpus(root))
	ctx := context.Background()

	// In-root read works.
	ok := read(ctx, readJSON(t, readArgs{Path: "doc.md"}))
	if ok.isError || !strings.Contains(ok.content, "42") {
		t.Errorf("read(doc.md) = %+v, want success containing '42'", ok)
	}

	refusals := []readArgs{
		{Path: "../secret.txt"},     // out of root
		{Path: "../../etc/passwd"},  // deeper escape
		{Path: ".git/config"},       // ignored
		{Path: ".shoka.disposable"}, // injected ignore
	}
	if runtime.GOOS != "windows" {
		refusals = append(refusals, readArgs{Path: "leak.txt"}) // symlink leaf
	}
	for _, a := range refusals {
		res := read(ctx, readJSON(t, a))
		if !res.isError {
			t.Errorf("read(%q) was accepted; want refusal", a.Path)
		}
		if strings.Contains(res.content, secret) {
			t.Errorf("read(%q) leaked the secret: %q", a.Path, res.content)
		}
	}
}

// TestReadTool_Ranged covers the offset/limit ranged read (shape for the future
// 368k single-file backlog).
func TestReadTool_Ranged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lines.txt"), "L0\nL1\nL2\nL3\nL4")
	g := NewGuard(root, nil)
	read := readDispatch(g, NewDirCorpus(root))
	ctx := context.Background()

	got := read(ctx, readJSON(t, readArgs{Path: "lines.txt", Offset: 1, Limit: 2}))
	if got.isError || got.content != "L1\nL2" {
		t.Errorf("ranged read = %+v, want 'L1\\nL2'", got)
	}
	all := read(ctx, readJSON(t, readArgs{Path: "lines.txt"}))
	if all.isError || all.content != "L0\nL1\nL2\nL3\nL4" {
		t.Errorf("full read = %+v, want all lines", all)
	}
}

// TestListTool_Security proves list omits ignored entries and symlinks, refuses
// listing outside the root, and never leaks the secret.
func TestListTool_Security(t *testing.T) {
	root, ignore, secret := fixtureCorpus(t)
	g := NewGuard(root, ignore)
	list := listDispatch(g, NewDirCorpus(root))
	ctx := context.Background()

	res := list(ctx, json.RawMessage(`{}`)) // root listing
	if res.isError {
		t.Fatalf("list(root) refused: %v", res.content)
	}
	for _, hidden := range []string{".git", ".shoka.disposable", "leak.txt"} {
		if strings.Contains(res.content, hidden) {
			t.Errorf("list(root) exposed %q: %q", hidden, res.content)
		}
	}
	for _, shown := range []string{"doc.md", "guide/"} {
		if !strings.Contains(res.content, shown) {
			t.Errorf("list(root) missing %q: %q", shown, res.content)
		}
	}
	if strings.Contains(res.content, secret) {
		t.Errorf("list leaked the secret: %q", res.content)
	}

	// Listing outside the root is refused.
	out := list(ctx, json.RawMessage(`{"dir":"../"}`))
	if !out.isError {
		t.Errorf("list(../) was accepted; want refusal")
	}
}

// TestSearchTool_Security proves the search tool returns in-root hits with a
// line offset, drops ignored/symlink/out-of-root hits, and never surfaces the
// secret — even though the secret lives in ignored files and an out-of-root
// symlink target that a naive grep would otherwise find.
func TestSearchTool_Security(t *testing.T) {
	root, ignore, secret := fixtureCorpus(t)
	// Add an in-root hit that mentions a unique token, on a known line.
	writeFile(t, filepath.Join(root, "guide", "topic.md"), "intro\nintro\nThe widget config lives here.\n")
	g := NewGuard(root, ignore)
	search := searchDispatch(g, NewDirCorpus(root))
	ctx := context.Background()

	// A content search finds the in-root file and reports the match line.
	res := search(ctx, json.RawMessage(`{"query":"widget config"}`))
	if res.isError {
		t.Fatalf("search refused: %v", res.content)
	}
	if !strings.Contains(res.content, "guide/topic.md") {
		t.Errorf("search missed the in-root hit: %q", res.content)
	}
	if !strings.Contains(res.content, "offset 2") {
		t.Errorf("search did not report the match line (offset 2): %q", res.content)
	}

	// Searching for the secret must surface NO hit (it lives only in ignored
	// files and an out-of-root symlink target — all guard-dropped).
	leak := search(ctx, json.RawMessage(`{"query":"`+secret+`"}`))
	if strings.Contains(leak.content, secret) {
		t.Errorf("search leaked the secret: %q", leak.content)
	}
	for _, hidden := range []string{".git", ".shoka.disposable", "leak.txt"} {
		if strings.Contains(leak.content, hidden) {
			t.Errorf("search surfaced an ignored/symlink path %q: %q", hidden, leak.content)
		}
	}
}

// TestSearchTool_RangedReadFlow proves the search->ranged-read shape: a search
// hit's offset feeds a bounded read that returns only the passage.
func TestSearchTool_RangedReadFlow(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 500; i++ {
		if i == 321 {
			b.WriteString("THE-NEEDLE is on this line\n")
		} else {
			b.WriteString("filler line\n")
		}
	}
	writeFile(t, filepath.Join(root, "big.md"), b.String())
	g := NewGuard(root, nil)
	corpus := NewDirCorpus(root)
	ctx := context.Background()

	hits, err := corpus.Search(ctx, "THE-NEEDLE", 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search = (%v, %v), want one hit", hits, err)
	}
	if hits[0].Offset != 321 {
		t.Errorf("hit offset = %d, want 321", hits[0].Offset)
	}
	// A read seeded at the hit offset returns a BOUNDED span, not the whole file.
	read := readDispatch(g, corpus)
	args, _ := json.Marshal(readArgs{Path: "big.md", Offset: hits[0].Offset, Limit: 1})
	r := read(ctx, args)
	if r.isError || strings.TrimSpace(r.content) != "THE-NEEDLE is on this line" {
		t.Errorf("ranged read = %q, want just the needle line", r.content)
	}
}

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
	read := readDispatch(g)
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
	read := readDispatch(g)
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
	list := listDispatch(g)
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

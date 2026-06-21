package librarian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Guard is the constraint kernel: every corpus access the tool-call loop
// dispatches passes through it BEFORE touching the filesystem. It enforces the
// three read-only librarian constraints — root-confinement, ignore-filtering,
// and symlink-skip — in Go, not in the prompt (B-49: out-of-bounds access is
// not a reachable hand). It is pure path/FS logic; no LLM, no go-git.
type Guard struct {
	root   string // absolute OS path of the corpus root
	ignore IgnoreMatcher
}

// NewGuard builds a Guard rooted at root (made absolute) with ".git/" plus the
// injected ignore patterns. A non-absolute or unresolvable root is taken
// as-is; relWithin still confines every access to it.
func NewGuard(root string, ignorePatterns []string) *Guard {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Guard{root: root, ignore: NewIgnoreMatcher(ignorePatterns)}
}

// Root returns the guard's absolute root.
func (g *Guard) Root() string { return g.root }

// Ignored reports whether the slash-relative path is ignored as the given kind.
func (g *Guard) Ignored(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	return g.ignore.Match(strings.Split(rel, "/"), isDir)
}

// Resolve validates path (relative to the root) for access as a file (isDir
// false) or directory (isDir true) and returns the full OS path plus the
// slash-relative path. It rejects, in order:
//
//   - lexical escape — absolute paths, "..", a leading "/" (relWithin);
//   - ignored paths — ".git/" and injected patterns;
//   - symlinks — any component along the path that is a symlink is refused
//     (never followed/resolved: the operator's B-73 correction — the librarian
//     never needs a link's target, so skipping, not EvalSymlinks, is correct).
//
// On rejection the loop turns the error into a tool_result error; no escape
// ever reaches the corpus adapter.
func (g *Guard) Resolve(path string, isDir bool) (full, rel string, err error) {
	full, rel, err = relWithin(g.root, path)
	if err != nil {
		return "", "", err
	}
	if g.Ignored(rel, isDir) {
		return "", "", fmt.Errorf("path is ignored: %q", path)
	}
	if err := g.rejectSymlinkComponents(rel); err != nil {
		return "", "", err
	}
	return full, rel, nil
}

// rejectSymlinkComponents Lstats each component from the root down and rejects
// the first symlink it finds. Lstat does not follow links, so this catches a
// symlinked leaf AND a symlinked intermediate directory without ever resolving
// a target. A not-yet-existing path is fine here (the adapter reports that).
func (g *Guard) rejectSymlinkComponents(rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	cur := g.root
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		sym, err := isSymlink(cur)
		if err != nil {
			// Non-existence is not a symlink violation; let the adapter
			// surface a clean "not found". Other stat errors propagate.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if sym {
			return fmt.Errorf("path is a symlink (skipped): %q", rel)
		}
	}
	return nil
}

// relWithin validates path traversal lexically and returns the full OS path and
// the slash-relative path. It rejects absolute paths, a ".." prefix, and a
// leading "/" on the computed relative path. This mirrors the lexical core of
// internal/storage/fs_git.go:504 (relWithin), re-implemented here so
// pkg/librarian imports no internal/storage. Lexical only — symlink-skip is a
// separate, deliberate step (see rejectSymlinkComponents).
func relWithin(root, path string) (string, string, error) {
	full := filepath.Join(root, path)
	rel, err := filepath.Rel(root, full)
	if filepath.IsAbs(path) || err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", "", fmt.Errorf("path escapes root: %q", path)
	}
	return full, filepath.ToSlash(rel), nil
}

// isSymlink reports whether the entry at osPath is a symlink, using Lstat so the
// link itself — not its target — is inspected.
func isSymlink(osPath string) (bool, error) {
	fi, err := os.Lstat(osPath)
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}

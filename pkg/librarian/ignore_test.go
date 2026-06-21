package librarian

import (
	"strings"
	"testing"
)

func split(p string) []string { return strings.Split(p, "/") }

func TestIgnoreMatcher_DefaultGitDir(t *testing.T) {
	m := NewIgnoreMatcher(nil) // only the default ".git/"

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{".git", true, true},            // the git dir itself
		{".git/config", false, true},    // a file under it
		{".git/refs/heads", true, true}, // a dir under it
		{"sub/.git", true, true},        // a nested git dir
		{"sub/.git/HEAD", false, true},  // a file under a nested git dir
		{"README.md", false, false},     // unrelated file
		{"gitignore", false, false},     // not ".git"
		{".github", true, false},        // not ".git"
	}
	for _, c := range cases {
		if got := m.Match(split(c.path), c.isDir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestIgnoreMatcher_InjectedPatterns(t *testing.T) {
	m := NewIgnoreMatcher([]string{
		".shoka*",      // glob prefix at any depth
		"node_modules", // basename dir at any depth
		"*.lock",       // glob suffix at any depth
		"/root-only",   // anchored to root
		"build/",       // directory-only
	})

	cases := []struct {
		name  string
		path  string
		isDir bool
		want  bool
	}{
		{"shoka disposable file", ".shoka.disposable", false, true},
		{"shoka ignore file", ".shokaignore", false, true},
		{"shoka nested", "a/b/.shoka.config", false, true},
		{"node_modules dir", "node_modules", true, true},
		{"node_modules nested", "web/node_modules", true, true},
		{"file under node_modules", "node_modules/pkg/index.js", false, true},
		{"lock file", "go.sum.lock", false, true},
		{"lock nested", "deep/dir/yarn.lock", false, true},
		{"plain go file", "main.go", false, false},
		{"anchored at root", "root-only", false, true},
		{"anchored NOT nested", "sub/root-only", false, false},
		{"dir-only matches dir", "build", true, true},
		{"dir-only file under it", "build/out.bin", false, true},
		{"dir-only does NOT match a plain file", "build", false, false},
	}
	for _, c := range cases {
		if got := m.Match(split(c.path), c.isDir); got != c.want {
			t.Errorf("%s: Match(%q, dir=%v) = %v, want %v", c.name, c.path, c.isDir, got, c.want)
		}
	}
}

func TestIgnoreMatcher_Negation(t *testing.T) {
	// Last matching pattern wins: ignore all *.lock, then re-include keep.lock.
	m := NewIgnoreMatcher([]string{"*.lock", "!keep.lock"})

	if !m.Match(split("go.lock"), false) {
		t.Errorf("go.lock should be ignored")
	}
	if m.Match(split("keep.lock"), false) {
		t.Errorf("keep.lock should be re-included by negation")
	}
}

func TestIgnoreMatcher_DoubleStar(t *testing.T) {
	m := NewIgnoreMatcher([]string{"a/**/secret"})

	cases := []struct {
		path string
		want bool
	}{
		{"a/secret", true},       // ** consumes zero
		{"a/b/secret", true},     // ** consumes one
		{"a/b/c/secret", true},   // ** consumes many
		{"x/a/b/secret", false},  // anchored: must start at a/
		{"a/b/secret/x", true},   // everything under a matched path
		{"a/b/notsecret", false}, // last segment must match
	}
	for _, c := range cases {
		if got := m.Match(split(c.path), false); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIgnoreMatcher_BlankAndComment(t *testing.T) {
	m := NewIgnoreMatcher([]string{"", "  ", "# a comment", "real.txt"})
	if !m.Match(split("real.txt"), false) {
		t.Errorf("real.txt should be ignored")
	}
	// Blank/comment lines contribute nothing.
	if m.Match(split("other.txt"), false) {
		t.Errorf("other.txt should not be ignored")
	}
}

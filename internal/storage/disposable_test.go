package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// writeDisposable writes a shoka.disposable file with the given lines.
func writeDisposable(t *testing.T, path string, lines ...string) {
	t.Helper()
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isDisposable runs the project's effective matcher against a slash path.
func isDisposable(t *testing.T, s *FSGitStorage, ns, proj, path string) bool {
	t.Helper()
	m, err := s.effectiveDisposable(ns, proj)
	if err != nil {
		t.Fatalf("effectiveDisposable: %v", err)
	}
	return m.Match(splitDisposablePath(path), false)
}

func TestDisposable_MissingFilesMatchNothing(t *testing.T) {
	s := newIdentityStorage(t)
	// No shoka.disposable files at any level: nothing is disposable, no error.
	if isDisposable(t, s, "ns", "proj", ".DS_Store") {
		t.Fatal("expected .DS_Store NOT disposable with no patterns")
	}
}

func TestDisposable_ShokaWidePatternMatches(t *testing.T) {
	s := newIdentityStorage(t)
	writeDisposable(t, filepath.Join(s.baseDir, "shoka.disposable"), ".DS_Store", "Thumbs.db")
	if !isDisposable(t, s, "ns", "proj", ".DS_Store") {
		t.Fatal("expected .DS_Store disposable via Shoka-wide pattern")
	}
	if isDisposable(t, s, "ns", "proj", "keep.md") {
		t.Fatal("expected keep.md NOT disposable")
	}
}

func TestDisposable_CommentsAndBlankLinesIgnored(t *testing.T) {
	s := newIdentityStorage(t)
	writeDisposable(t, filepath.Join(s.baseDir, "shoka.disposable"),
		"# OS junk", "", "   ", ".DS_Store")
	if !isDisposable(t, s, "ns", "proj", ".DS_Store") {
		t.Fatal("expected .DS_Store disposable; comments/blanks must be skipped")
	}
}

func TestDisposable_NamespaceLevelApplies(t *testing.T) {
	s := newIdentityStorage(t)
	writeDisposable(t, filepath.Join(s.baseDir, "ns", "shoka.disposable"), "*.tmp")
	if !isDisposable(t, s, "ns", "proj", "scratch.tmp") {
		t.Fatal("expected scratch.tmp disposable via namespace pattern")
	}
}

func TestDisposable_ProjectNegationOverridesWide(t *testing.T) {
	s := newIdentityStorage(t)
	// Shoka-wide disposes all *.log; project re-includes keep.log via !negation.
	writeDisposable(t, filepath.Join(s.baseDir, "shoka.disposable"), "*.log")
	writeDisposable(t, filepath.Join(s.baseDir, "ns", "proj.shoka.disposable"), "!keep.log")
	if !isDisposable(t, s, "ns", "proj", "debug.log") {
		t.Fatal("expected debug.log disposable via wide *.log")
	}
	if isDisposable(t, s, "ns", "proj", "keep.log") {
		t.Fatal("expected keep.log NOT disposable: project-level !keep.log must override wide")
	}
}

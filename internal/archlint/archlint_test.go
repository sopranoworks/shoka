package archlint

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// goGitImportPrefix matches go-git and every sub-package.
	goGitImportPrefix = "github.com/go-git/go-git"

	// allowedDir is the ONE directory tree permitted to import go-git: the
	// storage submodule owns all git access. The allowlist is an exact path
	// prefix, NOT a pattern, and there is deliberately NO _test.go exemption —
	// a production OR a test file outside this tree that imports go-git fails.
	// (Cross-package test helpers that legitimately need git live inside this
	// tree; tests elsewhere assert through the submodule's public API.)
	allowedDir = "internal/storage"
)

// skipDirs are directory names pruned from the module-wide scan: VCS, the JS web
// app, build output, and runtime data. testdata is pruned too — the go tool
// ignores it, so the deliberately-bad self-test fixture there is invisible to
// builds and to this scan (the self-test scans it explicitly instead).
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "web": true,
	"dist": true, "tmp": true, ".opencode": true, "testdata": true,
}

// scanGoGitImports walks walkRoot and returns the module-relative paths (relative
// to modRoot) of every .go file that imports go-git from outside allowedDir.
// pruneTestdata controls whether testdata subtrees are skipped (true for the
// real module scan; false when the self-test points walkRoot AT a testdata tree).
func scanGoGitImports(t *testing.T, walkRoot, modRoot string, pruneTestdata bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != walkRoot && skipDirs[d.Name()] && (pruneTestdata || d.Name() != "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // not our concern; the compiler reports unparseable files
		}
		importsGoGit := false
		for _, imp := range f.Imports {
			if strings.HasPrefix(strings.Trim(imp.Path.Value, `"`), goGitImportPrefix) {
				importsGoGit = true
				break
			}
		}
		if !importsGoGit {
			return nil
		}
		rel, rerr := filepath.Rel(modRoot, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel != allowedDir && !strings.HasPrefix(rel, allowedDir+"/") {
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", walkRoot, err)
	}
	return violations
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from CWD")
		}
		dir = parent
	}
}

// TestNoGoGitImportsOutsideStorage is the Anchor-2 guarantee: scanning the whole
// module, go-git must appear only under internal/storage. A violation fails the
// `go test ./...` gate — the build-fail enforcement the directive requires.
func TestNoGoGitImportsOutsideStorage(t *testing.T) {
	root := moduleRoot(t)
	violations := scanGoGitImports(t, root, root, true)
	if len(violations) > 0 {
		t.Fatalf("go-git imported outside %s/ (Anchor 1 violation):\n  %s\n\n"+
			"All git access must go through the storage submodule's business-intent API; "+
			"no go-git import is permitted elsewhere (2026-06-01 gitwrap directive).",
			allowedDir, strings.Join(violations, "\n  "))
	}
}

// TestScannerDetectsViolation is the linter's self-test: the deliberately-bad
// fixture under testdata/ imports go-git from a non-storage path. The go tool
// ignores testdata, so the fixture never breaks the build and the module-wide
// scan never sees it — but scanning it explicitly here MUST flag it, proving the
// detector actually fires (a green TestNoGoGitImportsOutsideStorage would be
// meaningless if the scanner could not detect a real violation).
func TestScannerDetectsViolation(t *testing.T) {
	root := moduleRoot(t)
	fixture := filepath.Join(root, "internal", "archlint", "testdata")
	violations := scanGoGitImports(t, fixture, root, false)
	if len(violations) == 0 {
		t.Fatal("self-test: scanner failed to flag the deliberately-bad go-git import fixture under testdata/")
	}
}

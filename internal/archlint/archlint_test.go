package archlint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// llmSDKImportPrefixes are the third-party LLM provider SDKs. Like go-git, they
// are confined to ONE directory — the librarian's LLM seam — so the provider
// SDKs never leak into the rest of the codebase; the loop and the rest of Shoka
// speak only the seam's neutral Message/Block/ToolDef types. Adding a provider
// means adding a client behind the seam, not a new SDK import elsewhere.
var llmSDKImportPrefixes = []string{
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/openai/openai-go",
	"google.golang.org/genai",
}

// llmSeamDir is the ONE directory tree permitted to import an LLM provider SDK.
const llmSeamDir = "pkg/librarian/llm"

// scanLLMSDKImports walks walkRoot and returns the module-relative paths of every
// .go file importing an LLM provider SDK from outside llmSeamDir. It mirrors
// scanGoGitImports; there is deliberately no _test.go exemption.
func scanLLMSDKImports(t *testing.T, walkRoot, modRoot string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != walkRoot && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		importsSDK := false
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, prefix := range llmSDKImportPrefixes {
				if p == prefix || strings.HasPrefix(p, prefix+"/") {
					importsSDK = true
					break
				}
			}
		}
		if !importsSDK {
			return nil
		}
		rel, rerr := filepath.Rel(modRoot, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel != llmSeamDir && !strings.HasPrefix(rel, llmSeamDir+"/") {
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", walkRoot, err)
	}
	return violations
}

// TestLLMSDKsConfinedToSeam guarantees every LLM provider SDK (anthropic-sdk-go,
// openai-go, and the genai SDK) is imported only under pkg/librarian/llm. A
// provider is added as a client behind the CreateMessage seam, never as a new SDK
// dependency leaking into the loop or the rest of Shoka. A violation fails the
// `go test ./...` gate.
func TestLLMSDKsConfinedToSeam(t *testing.T) {
	root := moduleRoot(t)
	violations := scanLLMSDKImports(t, root, root)
	if len(violations) > 0 {
		t.Fatalf("an LLM provider SDK is imported outside %s/:\n  %s\n\n"+
			"Provider SDKs must stay behind the librarian's CreateMessage seam; the loop "+
			"and the rest of Shoka speak only the seam's neutral types.",
			llmSeamDir, strings.Join(violations, "\n  "))
	}
}

// ----- Anchor 3: every git-ref write is atomic (2026-06-02 directive) -----
//
// The 2026-06-02 ref-write race was go-git's loose-ref write truncating the ref
// (O_TRUNC) before writing the new hash, which concurrent lock-free readers
// observed as an empty ref. The fix routes every ref write through the atomic
// temp+rename funnel in internal/storage/refwrite.go. This rule mechanically
// enforces that property: no file in internal/storage may write a ref through
// go-git — not via the storer (SetReference family) and not via the porcelain
// (Worktree.Commit / Reset / Checkout, Repository/Remote Push / Pull / Fetch /
// Clone), all of which reach the non-atomic setRefRwfs. The funnel itself writes
// the ref file with os primitives and calls NO go-git ref API, so the ban is
// total — there is no allowlisted file.
//
// (git.PlainInit / PlainOpen are repo constructors, not ref-write APIs; PlainInit
// writes the symbolic HEAD non-atomically but before any reader can observe the
// project, so it is deliberately NOT blocked.)

// refWriteStorerNames are go-git storer methods that write a ref. These selector
// names are go-git-specific, so a bare name match is precise (tier 1).
var refWriteStorerNames = map[string]bool{
	"SetReference":         true,
	"CheckAndSetReference": true,
	"RemoveReference":      true,
}

// refWritePorcelainNames are go-git porcelain methods that BUNDLE a ref write.
// Their names are common (bytes.Buffer.Reset, etc.), so they are flagged only
// when the call also references the go-git package in its arguments — every such
// porcelain call takes a go-git options value (git.CommitOptions, git.ResetOptions,
// …) (tier 2). A call with a variable options arg would evade this; that residual
// is acceptable (Anchor 1 confines go-git to internal/storage, the storer
// primitive is caught unconditionally by tier 1, and porcelain is conventionally
// written with an inline options literal).
var refWritePorcelainNames = map[string]bool{
	"Commit": true, "Reset": true, "Checkout": true,
	"Push": true, "Pull": true, "Fetch": true, "Clone": true,
}

const goGitImportPath = "github.com/go-git/go-git/v5"

// goGitLocalName returns the local identifier the file binds go-git's root
// package to (default "git", or an explicit import alias), or "" if the file does
// not import it. Tier-2 detection keys off this name appearing in a call's args.
func goGitLocalName(f *ast.File) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != goGitImportPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "git"
	}
	return ""
}

// argsReferenceGoGit reports whether any argument subtree references the go-git
// package by its local name (e.g. &git.ResetOptions{…} or git.HardReset).
func argsReferenceGoGit(args []ast.Expr, gogit string) bool {
	if gogit == "" {
		return false
	}
	found := false
	for _, a := range args {
		ast.Inspect(a, func(n ast.Node) bool {
			if found {
				return false
			}
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == gogit {
					found = true
					return false
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// scanRefWriteCalls walks walkRoot and returns "<relpath>:<line> <method>" for
// every go-git ref-write call found outside the atomic funnel.
func scanRefWriteCalls(t *testing.T, walkRoot, modRoot string, pruneTestdata bool) []string {
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
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // the compiler reports unparseable files
		}
		gogit := goGitLocalName(f)
		rel, rerr := filepath.Rel(modRoot, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			switch {
			case refWriteStorerNames[name]:
			case refWritePorcelainNames[name] && argsReferenceGoGit(call.Args, gogit):
			default:
				return true
			}
			line := fset.Position(call.Pos()).Line
			violations = append(violations, rel+":"+strconv.Itoa(line)+" "+name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", walkRoot, err)
	}
	return violations
}

// TestNoNonAtomicRefWritesInStorage is the Anchor-3 guarantee: scanning
// internal/storage, no file may write a git ref through go-git (storer or
// porcelain). A violation fails the `go test ./...` gate. The atomic funnel
// (refwrite.go) writes the ref file directly with os primitives and calls no
// go-git ref API, so there is no allowlisted file — the ban is total.
func TestNoNonAtomicRefWritesInStorage(t *testing.T) {
	root := moduleRoot(t)
	storageDir := filepath.Join(root, "internal", "storage")
	violations := scanRefWriteCalls(t, storageDir, root, true)
	if len(violations) > 0 {
		t.Fatalf("non-atomic go-git ref write(s) in internal/storage (Anchor 3 violation):\n  %s\n\n"+
			"Every ref write must go through the atomic temp+rename funnel "+
			"(internal/storage/refwrite.go: advanceHead/writeRefAtomic). go-git's storer "+
			"(SetReference family) and porcelain (Worktree.Commit/Reset/Checkout, Push/Pull/"+
			"Fetch/Clone) write refs non-atomically (O_TRUNC before write) and would re-open "+
			"the 2026-06-02 ref-write race (atomic-ref-write-enforcement directive).",
			strings.Join(violations, "\n  "))
	}
}

// TestRefWriteScannerDetectsViolation is the Anchor-3 rule's self-test: the
// deliberately-bad fixture under testdata/badrefwrite calls both a tier-1 storer
// ref write and tier-2 porcelain ref writes from outside the funnel. The go tool
// ignores testdata, so it never breaks the build and the storage scan never sees
// it — scanning it explicitly here MUST flag every violation, proving the
// detector fires for both tiers.
func TestRefWriteScannerDetectsViolation(t *testing.T) {
	root := moduleRoot(t)
	fixture := filepath.Join(root, "internal", "archlint", "testdata", "badrefwrite")
	violations := scanRefWriteCalls(t, fixture, root, false)
	// Expect: SetReference (tier 1) + Commit + Reset (tier 2) = 3.
	if len(violations) < 3 {
		t.Fatalf("self-test: scanner missed ref-write violations in the fixture; got %d:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
	var sawStorer, sawPorcelain bool
	for _, v := range violations {
		if strings.HasSuffix(v, " SetReference") {
			sawStorer = true
		}
		if strings.HasSuffix(v, " Commit") || strings.HasSuffix(v, " Reset") {
			sawPorcelain = true
		}
	}
	if !sawStorer || !sawPorcelain {
		t.Fatalf("self-test: scanner must flag both tiers (storer=%v, porcelain=%v):\n  %s",
			sawStorer, sawPorcelain, strings.Join(violations, "\n  "))
	}
}

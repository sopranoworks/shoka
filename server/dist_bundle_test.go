package server

import (
	"bytes"
	"io/fs"
	"testing"
)

// TestDistBundleHasIndexHTML verifies that the embedded server/dist contains an
// index.html — the SPA entry point. Without it the Go binary serves a bare
// directory listing ("assets/") instead of the application. This catches a build
// pipeline failure (vite build not run, dist/ cleared but not rebuilt, root-owned
// files blocking emptyOutDir) before the binary ships.
func TestDistBundleHasIndexHTML(t *testing.T) {
	data, err := fs.ReadFile(DistFS, "dist/index.html")
	if err != nil {
		t.Fatal("embedded dist is missing index.html — run `npm run build` in web/ before building the Go binary")
	}
	if len(data) == 0 {
		t.Fatal("embedded dist/index.html is empty")
	}
	if !bytes.Contains(data, []byte("<script")) {
		t.Error("dist/index.html does not contain a <script tag — the Vite build may be incomplete")
	}
}

// TestDistBundleHasNoStrayKanji guards B-31: a prior Web-UI session's Coder added
// a decorative literal kanji 蕉 (U+8549) to the brand. It was never in the design
// spec and the operator wants it gone. This walks the ACTUAL embedded server/dist
// tree (what the running server serves) and fails if the codepoint reappears in
// any asset — catching a reintroduction at the shipped-artifact level, independent
// of the source-level Vitest component tests. RED before the fix (the kanji was
// minified into dist/assets/index-*.js); GREEN after the rebuilt bundle drops it.
func TestDistBundleHasNoStrayKanji(t *testing.T) {
	needle := []byte(string(rune(0x8549))) // 蕉, UTF-8 E8 95 89

	checked := 0
	err := fs.WalkDir(DistFS, "dist", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(DistFS, path)
		if readErr != nil {
			return readErr
		}
		checked++
		if bytes.Contains(data, needle) {
			t.Errorf("embedded bundle file %q contains the stray kanji U+8549 (蕉); remove it from web/src and rebuild server/dist", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded dist: %v", err)
	}
	if checked == 0 {
		t.Fatal("no embedded dist files were checked — is server/dist built and embedded?")
	}
}

// TestDistBundleHasNoRepositoryWording guards B-31's terminology fix: the Web UI
// used GitHub-style "repository" wording (the list heading "Repositories", the
// brand label "Back to repositories", the sidebar "Choose a repository") even
// though Shoka's terms are project + namespace. This walks the ACTUAL embedded
// server/dist tree and fails if any of those specific removed phrases reappear
// in a shipped asset — catching a stale (un-rebuilt) bundle at the artifact
// level, independent of the source-level Vitest tests. The needles are the exact
// removed user-facing strings (not the bare word "repository"), so legitimate
// "project"/"namespace" copy never false-positives. RED before the fix (the
// phrases were minified into dist/assets/index-*.js); GREEN after the rebuilt
// bundle drops them.
func TestDistBundleHasNoRepositoryWording(t *testing.T) {
	needles := [][]byte{
		[]byte("Repositories"),         // the list heading
		[]byte("Choose a repository"),  // the sidebar empty-state prompt
		[]byte("Back to repositories"), // the brand label + project-page back link
	}

	checked := 0
	err := fs.WalkDir(DistFS, "dist", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(DistFS, path)
		if readErr != nil {
			return readErr
		}
		checked++
		for _, needle := range needles {
			if bytes.Contains(data, needle) {
				t.Errorf("embedded bundle file %q contains the removed repository wording %q; replace it with Shoka's project/namespace terms in web/src and rebuild server/dist", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded dist: %v", err)
	}
	if checked == 0 {
		t.Fatal("no embedded dist files were checked — is server/dist built and embedded?")
	}
}

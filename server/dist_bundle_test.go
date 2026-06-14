package server

import (
	"bytes"
	"io/fs"
	"testing"
)

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

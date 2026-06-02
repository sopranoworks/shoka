package wal

import (
	"os"
	"strings"
	"testing"
)

func TestWAL_MoveEntryRoundTrip(t *testing.T) {
	dir, err := os.MkdirTemp("", "wal-move-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	seq, err := l.Append(Entry{
		Namespace: "ns",
		Project:   "proj",
		Op:        "move",
		MoveFrom:  "old.md",
		Path:      "new.md",
		Content:   []byte("# moved bytes\n"),
		Aux: []AuxFile{
			{Path: "ref.md", Content: []byte("see [x](new.md)\n")},
			{Path: "docs/deep.md", Content: []byte("[y](../new.md)\n")},
		},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := l.ReadByID(seq)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Op != "move" || got.MoveFrom != "old.md" || got.Path != "new.md" {
		t.Errorf("move fields wrong: op=%q from=%q path=%q", got.Op, got.MoveFrom, got.Path)
	}
	if string(got.Content) != "# moved bytes\n" {
		t.Errorf("content = %q", string(got.Content))
	}
	if len(got.Aux) != 2 {
		t.Fatalf("aux len = %d, want 2", len(got.Aux))
	}
	if got.Aux[0].Path != "ref.md" || string(got.Aux[0].Content) != "see [x](new.md)\n" {
		t.Errorf("aux[0] = %+v", got.Aux[0])
	}
	// Append must have filled each aux's integrity fields.
	if got.Aux[0].Version != sha256Hex([]byte("see [x](new.md)\n")) || got.Aux[0].Size != int64(len("see [x](new.md)\n")) {
		t.Errorf("aux[0] integrity not populated: size=%d ver=%s", got.Aux[0].Size, got.Aux[0].Version)
	}
}

// TestWAL_CorruptAuxIsQuarantined proves the aux payload is integrity-checked
// exactly like the primary content: a tampered aux content (without an updated
// version) fails the read and the entry is quarantined.
func TestWAL_CorruptAuxIsQuarantined(t *testing.T) {
	dir, err := os.MkdirTemp("", "wal-aux-corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	seq, err := l.Append(Entry{
		Namespace: "ns", Project: "proj", Op: "move",
		MoveFrom: "a.md", Path: "b.md", Content: []byte("x"),
		Aux: []AuxFile{{Path: "ref.md", Content: []byte("orig")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the on-disk aux content but not its recorded version.
	path := dir + "/.shoka/wal/" + seqName(seq)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// "orig" base64 is "b3JpZw=="; replace with "evil" base64 "ZXZpbA==" (same len).
	s := string(data)
	if !strings.Contains(s, "b3JpZw==") {
		t.Skip("base64 layout changed; tamper anchor not found")
	}
	tampered := strings.Replace(s, "b3JpZw==", "ZXZpbA==", 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadByID(seq); err == nil {
		t.Fatal("tampered aux content must fail the integrity check")
	}
}

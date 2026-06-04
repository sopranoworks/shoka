package index

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func tmpIndexPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "proj.index.db")
}

func TestCreateOpenClose(t *testing.T) {
	p := tmpIndexPath(t)
	idx, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Meta is populated; marker starts empty.
	if v, _ := idx.Meta(MetaNamespace); v != "ns" {
		t.Errorf("namespace meta = %q, want ns", v)
	}
	if v, _ := idx.Meta(MetaProjectName); v != "proj" {
		t.Errorf("project meta = %q, want proj", v)
	}
	if c, _ := idx.LastIndexedCommit(); c != "" {
		t.Errorf("fresh LastIndexedCommit = %q, want empty", c)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen succeeds and preserves meta.
	idx2, err := Open(p)
	if err != nil {
		t.Fatalf("Open after create: %v", err)
	}
	defer idx2.Close()
	if v, _ := idx2.Meta(MetaProjectName); v != "proj" {
		t.Errorf("reopened project meta = %q, want proj", v)
	}
}

func TestCreateRejectsExisting(t *testing.T) {
	p := tmpIndexPath(t)
	idx, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	idx.Close()
	if _, err := Create(p, "ns", "proj"); err == nil {
		t.Fatal("Create over an existing file must fail")
	}
}

func TestOpenMissingIsErrNotFound(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent.index.db"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(missing) = %v, want ErrNotFound", err)
	}
}

func TestOpenGarbageIsErrCorrupt(t *testing.T) {
	p := tmpIndexPath(t)
	if err := os.WriteFile(p, []byte("this is not a bbolt database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open(garbage) = %v, want ErrCorrupt", err)
	}
}

func TestOpenSchemaMismatch(t *testing.T) {
	p := tmpIndexPath(t)
	idx, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the schema version, then reopen.
	if err := idx.SetMeta(MetaSchemaVersion, "999"); err != nil {
		t.Fatal(err)
	}
	idx.Close()
	_, err = Open(p)
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Open(schema mismatch) = %v, want ErrSchemaMismatch", err)
	}
}

func TestPutGetDeleteRecord(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.PutRecord("dir/file.md", IndexRecord{Etag: "abc"}); err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	rec, ok, err := idx.GetRecord("dir/file.md")
	if err != nil || !ok {
		t.Fatalf("GetRecord ok=%v err=%v, want true,nil", ok, err)
	}
	if rec.Etag != "abc" {
		t.Errorf("record etag = %q, want abc", rec.Etag)
	}

	// Path normalisation: a leading slash addresses the same record.
	if rec2, ok2, _ := idx.GetRecord("/dir/file.md"); !ok2 || rec2.Etag != "abc" {
		t.Errorf("normalised lookup ok=%v etag=%q", ok2, rec2.Etag)
	}

	// Upsert replaces.
	if err := idx.PutRecord("dir/file.md", IndexRecord{Etag: "def"}); err != nil {
		t.Fatal(err)
	}
	if rec3, _, _ := idx.GetRecord("dir/file.md"); rec3.Etag != "def" {
		t.Errorf("after upsert etag = %q, want def", rec3.Etag)
	}

	// Delete is idempotent.
	if err := idx.DeleteRecord("dir/file.md"); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if _, ok, _ := idx.GetRecord("dir/file.md"); ok {
		t.Error("record present after delete")
	}
	if err := idx.DeleteRecord("dir/file.md"); err != nil {
		t.Errorf("second DeleteRecord (idempotent) errored: %v", err)
	}
}

func TestReplaceAllRebuildsWholesaleAndAdvancesMarker(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Seed a stale record that must NOT survive a wholesale rebuild.
	_ = idx.PutRecord("stale.md", IndexRecord{Etag: "old"})

	records := map[string]IndexRecord{
		"a.md":       {Etag: "h1"},
		"sub/b.md":   {Etag: "h2"},
		"sub/c/d.md": {Etag: "h3"},
	}
	if err := idx.ReplaceAll(records, "commitHEAD"); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}

	if _, ok, _ := idx.GetRecord("stale.md"); ok {
		t.Error("stale record survived ReplaceAll")
	}
	for p, want := range records {
		got, ok, _ := idx.GetRecord(p)
		if !ok || got.Etag != want.Etag {
			t.Errorf("after rebuild %q ok=%v etag=%q, want %q", p, ok, got.Etag, want.Etag)
		}
	}
	if n, _ := idx.Count(); n != len(records) {
		t.Errorf("Count after rebuild = %d, want %d", n, len(records))
	}
	if c, _ := idx.LastIndexedCommit(); c != "commitHEAD" {
		t.Errorf("marker after rebuild = %q, want commitHEAD", c)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.SetLastIndexedCommit("deadbeef"); err != nil {
		t.Fatal(err)
	}
	if c, _ := idx.LastIndexedCommit(); c != "deadbeef" {
		t.Errorf("marker = %q, want deadbeef", c)
	}
}

// TestRecordForwardCompatibleDecode proves an old record (only "etag") decodes
// cleanly — the property that lets I2/I3 add fields without a migration. After I2
// added Bigrams, an I1-era record must still decode with Bigrams nil.
func TestRecordForwardCompatibleDecode(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.PutRecord("f.md", IndexRecord{Etag: "x"}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := idx.GetRecord("f.md")
	if err != nil || !ok || rec.Etag != "x" {
		t.Fatalf("decode: ok=%v err=%v etag=%q", ok, err, rec.Etag)
	}
	if rec.Bigrams != nil {
		t.Errorf("an I1-era record must decode with Bigrams nil, got %v", rec.Bigrams)
	}
}

// TestBigrams_OverlappingSortedDeduped pins the bigram set: overlapping rune
// 2-grams, lowercased, deduplicated, sorted.
func TestBigrams_OverlappingSortedDeduped(t *testing.T) {
	// "abab" → {"ab","ba"} (ab appears twice, deduped); sorted.
	got := Bigrams("abab")
	want := []string{"ab", "ba"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bigrams(abab) = %v, want %v", got, want)
	}
}

// TestBigrams_CaseFolded pins that Bigrams lowercases exactly like the verifier,
// so the index's "contains" equals SearchFiles' case-insensitive match.
func TestBigrams_CaseFolded(t *testing.T) {
	if !reflect.DeepEqual(Bigrams("HEllo"), Bigrams("hello")) {
		t.Fatalf("Bigrams must be case-folded: %v vs %v", Bigrams("HEllo"), Bigrams("hello"))
	}
}

// TestBigrams_JapaneseRuneAware pins CJK correctness: bigrams are over runes, not
// bytes, so a 3-rune Japanese word yields 2 multibyte 2-grams (never a split
// mid-rune).
func TestBigrams_JapaneseRuneAware(t *testing.T) {
	got := Bigrams("日本語") // runes: 日 本 語 → bigrams 日本, 本語
	want := []string{"日本", "本語"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bigrams(日本語) = %v, want %v", got, want)
	}
	for _, b := range got {
		if len([]rune(b)) != 2 {
			t.Errorf("bigram %q is not 2 runes", b)
		}
	}
}

// TestBigrams_ShortQueryHasNone pins the short-query rule's basis: a query shorter
// than 2 runes has no bigram (the caller then falls back to the full scan).
func TestBigrams_ShortQueryHasNone(t *testing.T) {
	if Bigrams("a") != nil {
		t.Errorf("a 1-rune string must have no bigram, got %v", Bigrams("a"))
	}
	if Bigrams("") != nil {
		t.Errorf("an empty string must have no bigram, got %v", Bigrams(""))
	}
	if Bigrams("あ") != nil { // 1 rune (multibyte)
		t.Errorf("a 1-rune multibyte string must have no bigram, got %v", Bigrams("あ"))
	}
}

// TestContainsAllBigrams pins the narrowing test: a record contains all of a
// query's bigrams iff every query bigram is present (the no-false-negative gate).
func TestContainsAllBigrams(t *testing.T) {
	rec := IndexRecord{Bigrams: Bigrams("the quick brown fox")}
	if !rec.ContainsAllBigrams(Bigrams("quick")) {
		t.Error("a substring's bigrams must all be present")
	}
	if rec.ContainsAllBigrams(Bigrams("zebra")) {
		t.Error("a non-substring with an absent bigram must be excluded")
	}
	// A bigram false positive: "kb" is absent, but even a query whose bigrams are
	// all present need not be a real substring — that is what truth-verify catches.
	// Here we only assert the gate's no-false-negative direction.
	if !rec.ContainsAllBigrams(nil) {
		t.Error("an empty query bigram set is vacuously contained")
	}
}

// TestBigrams_NoFalseNegativeOverCorpus is the property the fast path rests on:
// for any file content and any query that IS a case-insensitive substring of it,
// the content's bigrams contain all the query's bigrams. (Truth-verify handles the
// converse — false positives.)
func TestBigrams_NoFalseNegativeOverCorpus(t *testing.T) {
	contents := []string{"Hello, World", "日本語のテスト文書", "mixed 日本 and ASCII", "aaaa"}
	for _, c := range contents {
		rec := IndexRecord{Bigrams: Bigrams(c)}
		lc := strings.ToLower(c)
		runes := []rune(lc)
		// Every contiguous substring of length >= 2 must pass the gate.
		for i := 0; i < len(runes); i++ {
			for j := i + 2; j <= len(runes); j++ {
				sub := string(runes[i:j])
				if !rec.ContainsAllBigrams(Bigrams(sub)) {
					t.Fatalf("substring %q of %q failed the bigram gate (false negative)", sub, c)
				}
			}
		}
	}
}

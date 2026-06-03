package storage

import (
	"errors"
	"testing"
)

// --- splicePatch (the str_replace-style byte computation) ---

func TestSplicePatch_UniqueMatchReplaced(t *testing.T) {
	cur := []byte("alpha BETA gamma")
	got, err := splicePatch(cur, "BETA", "delta")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "alpha delta gamma" {
		t.Fatalf("got %q, want %q", got, "alpha delta gamma")
	}
}

func TestSplicePatch_ZeroMatchesIsNotFound(t *testing.T) {
	_, err := splicePatch([]byte("alpha beta"), "ZZZ", "x")
	var me *MatchError
	if !errors.As(err, &me) {
		t.Fatalf("want *MatchError, got %v", err)
	}
	if me.What != "old_string" || me.Count != 0 {
		t.Fatalf("got What=%q Count=%d, want old_string/0", me.What, me.Count)
	}
}

func TestSplicePatch_MultipleMatchesIsAmbiguous(t *testing.T) {
	_, err := splicePatch([]byte("x x x"), "x", "y")
	var me *MatchError
	if !errors.As(err, &me) {
		t.Fatalf("want *MatchError, got %v", err)
	}
	if me.What != "old_string" || me.Count != 3 {
		t.Fatalf("got What=%q Count=%d, want old_string/3", me.What, me.Count)
	}
}

func TestSplicePatch_NewStringMayBeEmpty(t *testing.T) {
	// Deleting a unique span is a valid patch.
	got, err := splicePatch([]byte("keep DROP keep"), " DROP", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "keep keep" {
		t.Fatalf("got %q, want %q", got, "keep keep")
	}
}

func TestSplicePatch_EmptyOldStringRejected(t *testing.T) {
	_, err := splicePatch([]byte("anything"), "", "x")
	if !errors.Is(err, ErrEmptyOldString) {
		t.Fatalf("want ErrEmptyOldString, got %v", err)
	}
}

func TestSplicePatch_OnlyFirstMatchSemanticsNeverGuess(t *testing.T) {
	// Two matches must NOT be silently resolved to the first; it is an error.
	_, err := splicePatch([]byte("the status: a\nthe status: b"), "status:", "STATUS:")
	var me *MatchError
	if !errors.As(err, &me) || me.Count != 2 {
		t.Fatalf("want ambiguous (2), got %v", err)
	}
}

// --- spliceAppend (end / before / after with a unique anchor) ---

func TestSpliceAppend_EndAppendsVerbatim(t *testing.T) {
	got, err := spliceAppend([]byte("line1\n"), []byte("line2\n"), "end", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "line1\nline2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSpliceAppend_EmptyPositionDefaultsToEnd(t *testing.T) {
	got, err := spliceAppend([]byte("a"), []byte("b"), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "ab" {
		t.Fatalf("got %q, want %q", got, "ab")
	}
}

func TestSpliceAppend_EndInsertsNoNewline(t *testing.T) {
	// Verbatim insertion: the server adds no separator. Caller owns newlines.
	got, _ := spliceAppend([]byte("no-trailing-nl"), []byte("X"), "end", "")
	if string(got) != "no-trailing-nlX" {
		t.Fatalf("got %q, want %q", got, "no-trailing-nlX")
	}
}

func TestSpliceAppend_BeforeAnchorInsertsImmediatelyBefore(t *testing.T) {
	cur := []byte("head\n## Cross-cutting\ntail\n")
	got, err := spliceAppend(cur, []byte("### NEW\n"), "before", "## Cross-cutting")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "head\n### NEW\n## Cross-cutting\ntail\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSpliceAppend_AfterAnchorInsertsImmediatelyAfter(t *testing.T) {
	cur := []byte("head\nANCHOR\ntail\n")
	got, err := spliceAppend(cur, []byte("INSERT\n"), "after", "ANCHOR\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "head\nANCHOR\nINSERT\ntail\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSpliceAppend_BeforeZeroMatchesIsNotFound(t *testing.T) {
	_, err := spliceAppend([]byte("abc"), []byte("x"), "before", "ZZZ")
	var me *MatchError
	if !errors.As(err, &me) || me.What != "anchor" || me.Count != 0 {
		t.Fatalf("want anchor not-found, got %v", err)
	}
}

func TestSpliceAppend_AfterMultipleMatchesIsAmbiguous(t *testing.T) {
	_, err := spliceAppend([]byte("a\na\na\n"), []byte("x"), "after", "a")
	var me *MatchError
	if !errors.As(err, &me) || me.What != "anchor" || me.Count != 3 {
		t.Fatalf("want anchor ambiguous(3), got %v", err)
	}
}

func TestSpliceAppend_BeforeRequiresAnchor(t *testing.T) {
	_, err := spliceAppend([]byte("abc"), []byte("x"), "before", "")
	if !errors.Is(err, ErrAnchorRequired) {
		t.Fatalf("want ErrAnchorRequired, got %v", err)
	}
}

func TestSpliceAppend_EndRejectsAnchor(t *testing.T) {
	// Stricter choice: anchor with position:end is a typed error, not a silent ignore.
	_, err := spliceAppend([]byte("abc"), []byte("x"), "end", "abc")
	if !errors.Is(err, ErrAnchorWithEnd) {
		t.Fatalf("want ErrAnchorWithEnd, got %v", err)
	}
}

func TestSpliceAppend_InvalidPositionRejected(t *testing.T) {
	_, err := spliceAppend([]byte("abc"), []byte("x"), "sideways", "")
	if !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("want ErrInvalidPosition, got %v", err)
	}
}

package opmeta

import "testing"

// TestFormatGate covers the format gate (Valid): the schema's accepted and
// rejected shapes.
func TestFormatGate(t *testing.T) {
	cases := []struct {
		name string
		m    Meta
		want bool
	}{
		{"delete ok", Meta{Op: OpDelete, Path: "a.md"}, true},
		{"write ok", Meta{Op: OpWrite, Path: "a.md"}, true},
		{"move ok", Meta{Op: OpMove, Path: "b.md", From: "a.md"}, true},
		{"empty path", Meta{Op: OpDelete, Path: ""}, false},
		{"unknown op", Meta{Op: "rename", Path: "a.md"}, false},
		{"delete with from", Meta{Op: OpDelete, Path: "a.md", From: "x"}, false},
		{"write with from", Meta{Op: OpWrite, Path: "a.md", From: "x"}, false},
		{"move without from", Meta{Op: OpMove, Path: "b.md"}, false},
	}
	for _, c := range cases {
		if got := c.m.Valid(); got != c.want {
			t.Errorf("%s: Valid()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestTrailerRoundTrip: a rendered trailer parses back to the same Meta, even
// embedded in a realistic multi-line message with identity trailers above it.
func TestTrailerRoundTrip(t *testing.T) {
	for _, m := range []Meta{
		{Op: OpDelete, Path: "docs/a.md"},
		{Op: OpMove, Path: "docs/b.md", From: "docs/a.md"},
		{Op: OpWrite, Path: "x.md"},
	} {
		msg := "Delete docs/a.md\n\n" +
			"Shoka-User: Op <op@shoka.local>\n" +
			"Shoka-Agent: agent\n" +
			Trailer(m)
		got, ok := Parse(msg)
		if !ok {
			t.Fatalf("Parse failed for %+v", m)
		}
		if got != m {
			t.Errorf("round trip: got %+v want %+v", got, m)
		}
	}
}

// TestParseIgnoresBadMetadata: absent, malformed, and schema-violating trailers
// all parse as "absent" (ok=false) — the caller then falls back to raw diff.
func TestParseIgnoresBadMetadata(t *testing.T) {
	cases := []string{
		"Update x.md\n\nShoka-User: Op <op@shoka.local>\n",              // no Shoka-Op line
		"Update x.md\n\nShoka-Op: {not valid json\n",                    // malformed JSON
		"Update x.md\n\nShoka-Op: {\"op\":\"rename\",\"path\":\"x\"}\n", // unknown op (schema)
		"Update x.md\n\nShoka-Op: {\"op\":\"move\",\"path\":\"x\"}\n",   // move without from
		"",
	}
	for _, msg := range cases {
		if _, ok := Parse(msg); ok {
			t.Errorf("Parse should have rejected: %q", msg)
		}
	}
}

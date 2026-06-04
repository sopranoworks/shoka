package storage

import (
	"reflect"
	"testing"
)

// scanOutboundLinks is the reverse-link extractor (I3). It must resolve each
// inline link/image destination to the SAME project-relative target that
// makeRewrite resolves to and compares against movedSrc, so the reverse-link
// index's "R links to T" agrees with the rewriter's "R's link resolves to
// movedSrc" by construction.
func TestScanOutboundLinks(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		referrer string
		want     []string
	}{
		{
			name:     "no links",
			content:  "# Title\n\nplain prose, no links.\n",
			referrer: "a.md",
			want:     nil,
		},
		{
			name:     "simple relative link resolves to project-relative target",
			content:  "see [doc](old.md) here",
			referrer: "a.md",
			want:     []string{"old.md"},
		},
		{
			name:     "relative with parent traversal resolves up",
			content:  "[up](../old.md)",
			referrer: "docs/a.md",
			want:     []string{"old.md"},
		},
		{
			name:     "relative in subdir resolves under referrer dir",
			content:  "[d](deep.md)",
			referrer: "docs/a.md",
			want:     []string{"docs/deep.md"},
		},
		{
			name:     "project-absolute resolves without leading slash",
			content:  "[abs](/old.md)",
			referrer: "deep/dir/a.md",
			want:     []string{"old.md"},
		},
		{
			name:     "anchor is stripped from the target",
			content:  "[sec](old.md#section-2)",
			referrer: "a.md",
			want:     []string{"old.md"},
		},
		{
			name:     "image destinations are included",
			content:  "![alt](pic.md)",
			referrer: "a.md",
			want:     []string{"pic.md"},
		},
		{
			name:     "inline code span is protected",
			content:  "use `[x](old.md)` literally",
			referrer: "a.md",
			want:     nil,
		},
		{
			name:     "fenced code block is protected",
			content:  "```\n[x](old.md)\n```\n",
			referrer: "a.md",
			want:     nil,
		},
		{
			name:     "external url is excluded",
			content:  "[ext](https://example.com/old.md)",
			referrer: "a.md",
			want:     nil,
		},
		{
			name:     "pure same-page anchor is excluded",
			content:  "[here](#section)",
			referrer: "a.md",
			want:     nil,
		},
		{
			name:     "multiple targets deduped and sorted",
			content:  "[a](z.md) [b](./z.md) [c](a.md#x) ![i](m.md)",
			referrer: "a.md",
			want:     []string{"a.md", "m.md", "z.md"},
		},
		{
			name:     "percent-encoded space decodes to the real target",
			content:  "[s](my%20doc.md)",
			referrer: "a.md",
			want:     []string{"my doc.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanOutboundLinks([]byte(tc.content), tc.referrer)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("scanOutboundLinks = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScanOutboundLinks_AgreesWithRewriter is the by-construction guarantee: a
// file's outbound target set (what the reverse-link index records) contains T
// if and only if rewriteLinks would rewrite that file for a move of T — so the
// healthy index lookup and the truth-scan rewrite never disagree about who
// references what.
func TestScanOutboundLinks_AgreesWithRewriter(t *testing.T) {
	corpus := []struct {
		referrer string
		content  string
	}{
		{"a.md", "[x](old.md) and [y](other.md)"},
		{"docs/b.md", "[up](../old.md) [sib](sibling.md) [abs](/top.md)"},
		{"c.md", "no links at all"},
		{"d.md", "`[code](old.md)` protected, [real](old.md) not"},
		{"e.md", "[ext](https://x.com/old.md) [anchor](#h)"},
	}
	const dst = "moved/new.md"
	for _, f := range corpus {
		targets := scanOutboundLinks([]byte(f.content), f.referrer)
		targetSet := map[string]bool{}
		for _, tg := range targets {
			targetSet[tg] = true
		}
		// For every target the extractor found, the rewriter must rewrite it.
		for _, tg := range targets {
			if _, n := rewriteLinks([]byte(f.content), f.referrer, tg, dst); n == 0 {
				t.Errorf("%s: extractor found target %q but rewriter rewrote nothing", f.referrer, tg)
			}
		}
		// And the rewriter must NOT rewrite any target the extractor did not find
		// (probe a few plausible non-targets).
		for _, probe := range []string{"old.md", "other.md", "sibling.md", "top.md", "absent.md"} {
			_, n := rewriteLinks([]byte(f.content), f.referrer, probe, dst)
			if n > 0 && !targetSet[probe] {
				t.Errorf("%s: rewriter rewrote target %q the extractor did not report", f.referrer, probe)
			}
		}
	}
}

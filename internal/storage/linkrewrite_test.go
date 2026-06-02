package storage

import "testing"

func TestRewriteLinks(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		referrer  string
		src, dst  string
		wantCount int
		wantOut   string
	}{
		{
			name:     "no links",
			content:  "# Title\n\nplain prose, no links here.\n",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 0, wantOut: "# Title\n\nplain prose, no links here.\n",
		},
		{
			name:     "simple relative inbound link",
			content:  "see [the doc](old.md) for details",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: "see [the doc](new.md) for details",
		},
		{
			name:     "relative with parent traversal",
			content:  "[up](../old.md)",
			referrer: "docs/a.md", src: "old.md", dst: "sub/new.md",
			wantCount: 1, wantOut: "[up](../sub/new.md)",
		},
		{
			name:     "project-absolute link preserves absolute form",
			content:  "[abs](/old.md)",
			referrer: "deep/dir/a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: "[abs](/new.md)",
		},
		{
			name:     "anchor preserved",
			content:  "[sec](old.md#section-2)",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: "[sec](new.md#section-2)",
		},
		{
			name:     "image link rewritten too",
			content:  "![alt](old.md)",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: "![alt](new.md)",
		},
		{
			name:     "inline code span is protected",
			content:  "use `[x](old.md)` literally",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 0, wantOut: "use `[x](old.md)` literally",
		},
		{
			name:     "fenced code block is protected",
			content:  "```\n[x](old.md)\n```\n",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 0, wantOut: "```\n[x](old.md)\n```\n",
		},
		{
			name:     "non-matching link untouched",
			content:  "[other](unrelated.md) and [hit](old.md)",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: "[other](unrelated.md) and [hit](new.md)",
		},
		{
			name:     "external url untouched",
			content:  "[ext](https://example.com/old.md)",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 0, wantOut: "[ext](https://example.com/old.md)",
		},
		{
			name:     "multiple matches in one file",
			content:  "[a](old.md) [b](./old.md) [c](old.md#x)",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 3, wantOut: "[a](new.md) [b](new.md) [c](new.md#x)",
		},
		{
			name:     "angle-bracket destination form preserved",
			content:  "[sp](<old.md>)",
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: "[sp](<new.md>)",
		},
		{
			name:     "title after destination preserved",
			content:  `[t](old.md "the title")`,
			referrer: "a.md", src: "old.md", dst: "new.md",
			wantCount: 1, wantOut: `[t](new.md "the title")`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, n := rewriteLinks([]byte(tc.content), tc.referrer, tc.src, tc.dst)
			if n != tc.wantCount {
				t.Errorf("count = %d, want %d", n, tc.wantCount)
			}
			if string(out) != tc.wantOut {
				t.Errorf("output =\n%q\nwant\n%q", string(out), tc.wantOut)
			}
		})
	}
}

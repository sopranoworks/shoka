package markdown

import (
	"strings"
	"testing"
)

func TestParse_Frontmatter(t *testing.T) {
	content := `---
title: My Doc
summary: A short summary
status: active
---
# The Heading

The first paragraph of the body.

Second paragraph.
`
	s := Parse(content)
	if s.Frontmatter["title"] != "My Doc" {
		t.Fatalf("title = %v, want My Doc", s.Frontmatter["title"])
	}
	if s.Frontmatter["status"] != "active" {
		t.Fatalf("status = %v, want active", s.Frontmatter["status"])
	}
	if s.Heading != "The Heading" {
		t.Fatalf("heading = %q, want 'The Heading'", s.Heading)
	}
	if s.Excerpt != "The first paragraph of the body." {
		t.Fatalf("excerpt = %q", s.Excerpt)
	}
}

func TestParse_MalformedFrontmatter_NoCrash(t *testing.T) {
	content := `---
a: b: c
not valid yaml
---
# Body Heading

Body text here.
`
	s := Parse(content) // must not panic
	if len(s.Frontmatter) != 0 {
		t.Fatalf("expected empty frontmatter on malformed YAML, got %v", s.Frontmatter)
	}
	if s.Heading != "Body Heading" {
		t.Fatalf("heading = %q, want 'Body Heading'", s.Heading)
	}
	if s.Excerpt != "Body text here." {
		t.Fatalf("excerpt = %q", s.Excerpt)
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	content := "# Just A Title\n\nSome content.\n"
	s := Parse(content)
	if len(s.Frontmatter) != 0 {
		t.Fatalf("expected empty frontmatter, got %v", s.Frontmatter)
	}
	if s.Heading != "Just A Title" {
		t.Fatalf("heading = %q", s.Heading)
	}
	if s.Excerpt != "Some content." {
		t.Fatalf("excerpt = %q", s.Excerpt)
	}
}

func TestParse_NoHeading(t *testing.T) {
	s := Parse("just some text with no heading\n")
	if s.Heading != "" {
		t.Fatalf("expected empty heading, got %q", s.Heading)
	}
	if s.Excerpt != "just some text with no heading" {
		t.Fatalf("excerpt = %q", s.Excerpt)
	}
}

func TestParse_ExcerptCappedForHugeFile(t *testing.T) {
	// A huge single paragraph with no blank lines or headings.
	huge := strings.Repeat("la ", 5000) // ~15000 chars
	s := Parse(huge)
	if n := len([]rune(s.Excerpt)); n > MaxExcerptRunes {
		t.Fatalf("excerpt length %d exceeds cap %d", n, MaxExcerptRunes)
	}
}

func TestParse_ExcerptCappedWithFrontmatterAndHeading(t *testing.T) {
	huge := "---\ntitle: X\n---\n# H\n\n" + strings.Repeat("word ", 5000)
	s := Parse(huge)
	if n := len([]rune(s.Excerpt)); n > MaxExcerptRunes {
		t.Fatalf("excerpt length %d exceeds cap %d", n, MaxExcerptRunes)
	}
}

// Package markdown provides lightweight, dependency-free extraction of a
// Markdown document's YAML frontmatter, first heading, and a short excerpt.
// It is deliberately forgiving: malformed frontmatter yields an empty map
// rather than an error, and the excerpt is always capped so that summarizing a
// huge file never returns a large amount of body content.
package markdown

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxExcerptRunes bounds the excerpt length (in runes, to behave well with
// multibyte content such as Japanese).
const MaxExcerptRunes = 200

// Summary is the extracted, context-efficient view of a Markdown document.
type Summary struct {
	Frontmatter map[string]any `json:"frontmatter"`
	Heading     string         `json:"heading"`
	Excerpt     string         `json:"excerpt"`
}

// Parse extracts the frontmatter, first heading, and first-paragraph excerpt
// from content. It never returns the full body and never panics on malformed
// input.
func Parse(content string) Summary {
	out := Summary{Frontmatter: map[string]any{}}

	body := content
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && strings.TrimRight(lines[0], "\r") == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimRight(lines[i], "\r") == "---" {
				var fm map[string]any
				if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &fm); err == nil && fm != nil {
					out.Frontmatter = fm
				}
				body = strings.Join(lines[i+1:], "\n")
				break
			}
		}
		// No closing fence: treat the whole content as body (no frontmatter).
	}

	out.Heading = firstHeading(body)
	out.Excerpt = firstExcerpt(body)
	return out
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			t = strings.TrimSpace(strings.TrimLeft(t, "#"))
			if t != "" {
				return t
			}
		}
	}
	return ""
}

func firstExcerpt(body string) string {
	var para []string
	started := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			if started {
				break
			}
			continue
		}
		if strings.HasPrefix(t, "#") {
			if started {
				break
			}
			continue // skip headings preceding the first paragraph
		}
		para = append(para, t)
		started = true
	}

	excerpt := strings.Join(para, " ")
	if excerpt == "" {
		// No clear paragraph boundary: fall back to the start of the body.
		excerpt = strings.TrimSpace(body)
	}
	return truncateRunes(excerpt, MaxExcerptRunes)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

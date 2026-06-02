package storage

import (
	"bytes"
	"net/url"
	"path"
	"path/filepath"
	"sort"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// linkrewrite.go implements the internal markdown link rewriting that rides a
// move (the move-file directive §1.4, the Obsidian signature feature). When a
// file moves from movedSrc to movedDst, every *other* markdown file that links
// to movedSrc is rewritten so the reference follows — all inside the move's
// single atomic commit.
//
// Why a real parser: the one hard requirement is "a markdown link inside a code
// block (or code span, or raw HTML) must NOT be rewritten." goldmark is the
// authority on which byte ranges are code/HTML; the destination spans are then
// located by a deterministic CommonMark inline-link scanner that skips those
// protected ranges, and rewritten by an exact byte-splice so every non-link byte
// (including the code that was protected) is preserved verbatim. We do not regex
// the document.
//
// Scope (per the directive's explicit pattern list): inline links `[text](dest)`
// and inline images `![alt](dest)`. Reference-style links (`[t][ref]` with a
// `[ref]: dest` definition) and autolinks (`<https://…>`) are not in the
// directive's `[text](path)` / `![alt](path)` pattern set; they are not
// rewritten. The moved file's OWN outbound links are intentionally left untouched
// (operator decision: preserve `git log --follow` by keeping the moved blob
// byte-identical; inbound references only).

// span is a half-open byte range [start, stop) in a source document.
type span struct{ start, stop int }

// linkMD is the goldmark instance used only to locate protected (code/HTML)
// ranges. Plain CommonMark core — no extensions — matches how the rest of Shoka
// treats markdown (the web client renders CommonMark via react-markdown).
var linkMD = goldmark.New()

// rewriteLinks returns referrer's content with every inline link/image whose
// destination resolves to movedSrc repointed at movedDst, and the count of links
// changed. referrerPath, movedSrc, and movedDst are all project-relative
// slash paths. When nothing matches it returns the input bytes unchanged and 0,
// so callers can cheaply detect "no rewrite needed". The returned slice is always
// a fresh copy when n > 0.
func rewriteLinks(content []byte, referrerPath, movedSrc, movedDst string) ([]byte, int) {
	protected := protectedRanges(content)
	rewrites := scanRewrites(content, protected, referrerPath, movedSrc, movedDst)
	if len(rewrites) == 0 {
		return content, 0
	}
	// Apply splices left to right into a fresh buffer.
	sort.Slice(rewrites, func(i, j int) bool { return rewrites[i].start < rewrites[j].start })
	var out bytes.Buffer
	out.Grow(len(content) + 32)
	cursor := 0
	for _, rw := range rewrites {
		out.Write(content[cursor:rw.start])
		out.WriteString(rw.replacement)
		cursor = rw.stop
	}
	out.Write(content[cursor:])
	return out.Bytes(), len(rewrites)
}

// rewrite is one destination-span replacement.
type rewrite struct {
	start, stop int
	replacement string
}

// protectedRanges parses content with goldmark and returns the byte ranges that
// must never be treated as containing a rewritable link: fenced/indented code
// blocks, HTML blocks, inline code spans, and inline raw HTML. The ranges are
// returned sorted by start.
func protectedRanges(content []byte) []span {
	doc := linkMD.Parser().Parse(text.NewReader(content))
	var ranges []span
	addSeg := func(s text.Segment) {
		if s.Stop > s.Start {
			ranges = append(ranges, span{s.Start, s.Stop})
		}
	}
	addLines := func(n ast.Node) {
		ls := n.Lines()
		if ls == nil {
			return
		}
		for i := 0; i < ls.Len(); i++ {
			addSeg(ls.At(i))
		}
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.HTMLBlock:
			addLines(n)
		case *ast.CodeSpan:
			// A code span's content is its child Text nodes' segments.
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if tn, ok := c.(*ast.Text); ok {
					addSeg(tn.Segment)
				}
			}
		case *ast.RawHTML:
			if t.Segments != nil {
				for i := 0; i < t.Segments.Len(); i++ {
					addSeg(t.Segments.At(i))
				}
			}
		}
		return ast.WalkContinue, nil
	})
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	return ranges
}

// inAnyRange reports whether pos falls inside any protected range. ranges is
// sorted by start; a linear scan is fine for the small counts in real documents.
func inAnyRange(ranges []span, pos int) bool {
	for _, r := range ranges {
		if pos < r.start {
			return false
		}
		if pos < r.stop {
			return true
		}
	}
	return false
}

// scanRewrites walks content as a CommonMark inline stream, finds inline link /
// image destinations outside the protected ranges, and records a rewrite for
// each whose path resolves to movedSrc. The bracket stack mirrors how CommonMark
// pairs `[` … `]`; an image's leading `!` is irrelevant to the destination, so
// links and images are handled identically.
func scanRewrites(content []byte, protected []span, referrerPath, movedSrc, movedDst string) []rewrite {
	var openers []int // positions of unmatched '['
	var rewrites []rewrite
	n := len(content)
	for i := 0; i < n; {
		c := content[i]
		if c == '\\' { // escaped char: skip both
			i += 2
			continue
		}
		switch c {
		case '[':
			if !inAnyRange(protected, i) {
				openers = append(openers, i)
			}
			i++
		case ']':
			if len(openers) > 0 && !inAnyRange(protected, i) {
				openers = openers[:len(openers)-1] // pop the matching '['
				if i+1 < n && content[i+1] == '(' && !inAnyRange(protected, i+1) {
					pStart, pStop, after, ok := parseInlineDestination(content, i+2)
					if ok {
						if rw, matched := makeRewrite(content, pStart, pStop, referrerPath, movedSrc, movedDst); matched {
							rewrites = append(rewrites, rw)
						}
						i = after
						continue
					}
				}
			}
			i++
		default:
			i++
		}
	}
	return rewrites
}

// parseInlineDestination parses a CommonMark inline-link destination starting at
// j (the byte just after the '('). It returns the byte span of the destination
// (path + optional #anchor, excluding any surrounding angle brackets), the index
// just past the closing ')', and ok. It tolerates an optional title and balanced
// parens, per CommonMark.
func parseInlineDestination(s []byte, j int) (pathStart, pathStop, after int, ok bool) {
	n := len(s)
	for j < n && isMDSpace(s[j]) {
		j++
	}
	if j >= n {
		return 0, 0, 0, false
	}
	if s[j] == '<' {
		// Angle-bracket form: <dest> — no '>' or newline inside.
		k := j + 1
		for k < n && s[k] != '>' && s[k] != '\n' {
			if s[k] == '\\' {
				k += 2
				continue
			}
			k++
		}
		if k >= n || s[k] != '>' {
			return 0, 0, 0, false
		}
		pathStart, pathStop = j+1, k
		after, ok = closingParen(s, k+1)
		return pathStart, pathStop, after, ok
	}
	// Bare form: until whitespace (title may follow) or an unbalanced ')'.
	depth := 0
	k := j
	for k < n {
		ch := s[k]
		if ch == '\\' {
			k += 2
			continue
		}
		if isMDSpace(ch) {
			break
		}
		if ch == '(' {
			depth++
		} else if ch == ')' {
			if depth == 0 {
				break
			}
			depth--
		}
		k++
	}
	pathStart, pathStop = j, k
	after, ok = closingParen(s, k)
	return pathStart, pathStop, after, ok
}

// closingParen scans an optional title then the required ')', returning the index
// just past it. m is positioned right after the destination.
func closingParen(s []byte, m int) (after int, ok bool) {
	n := len(s)
	for m < n && isMDSpace(s[m]) {
		m++
	}
	if m < n && (s[m] == '"' || s[m] == '\'' || s[m] == '(') {
		closer := byte(')')
		if s[m] == '"' {
			closer = '"'
		} else if s[m] == '\'' {
			closer = '\''
		}
		m++
		for m < n && s[m] != closer {
			if s[m] == '\\' {
				m += 2
				continue
			}
			m++
		}
		if m < n {
			m++ // consume the title closer
		}
	}
	for m < n && isMDSpace(s[m]) {
		m++
	}
	if m >= n || s[m] != ')' {
		return 0, false
	}
	return m + 1, true
}

func isMDSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// makeRewrite decides whether the destination span [pStart,pStop) points at
// movedSrc and, if so, builds the replacement (new path, anchor preserved). The
// destination's form (project-absolute "/x" vs relative) is preserved.
func makeRewrite(content []byte, pStart, pStop int, referrerPath, movedSrc, movedDst string) (rewrite, bool) {
	dest := content[pStart:pStop]
	// Split off a #fragment (preserved verbatim).
	rawPath := dest
	anchor := []byte(nil)
	if h := bytes.IndexByte(dest, '#'); h >= 0 {
		rawPath = dest[:h]
		anchor = dest[h:]
	}
	if len(rawPath) == 0 {
		return rewrite{}, false // pure same-page anchor
	}
	decoded := rawPath
	if d, err := url.PathUnescape(string(rawPath)); err == nil {
		decoded = []byte(d)
	}
	// External (has a URL scheme like http: or mailto:) → never a project file.
	if u, err := url.Parse(string(decoded)); err == nil && u.Scheme != "" {
		return rewrite{}, false
	}
	absolute := decoded[0] == '/'
	var resolved string
	if absolute {
		resolved = path.Clean(string(decoded[1:]))
	} else {
		resolved = path.Clean(path.Join(path.Dir(referrerPath), string(decoded)))
	}
	if resolved != movedSrc {
		return rewrite{}, false
	}
	// Build the new destination in the same form the original used.
	var newPath string
	if absolute {
		newPath = "/" + movedDst
	} else {
		rel, err := filepath.Rel(path.Dir(referrerPath), movedDst)
		if err != nil {
			return rewrite{}, false
		}
		newPath = filepath.ToSlash(rel)
	}
	// Re-encode spaces (and other characters that were percent-encoded) only if the
	// original was encoded; otherwise keep the path human-readable.
	if !bytes.Equal(rawPath, decoded) {
		newPath = (&url.URL{Path: newPath}).EscapedPath()
	}
	return rewrite{start: pStart, stop: pStop, replacement: newPath + string(anchor)}, true
}

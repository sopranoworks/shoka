package librarian

import (
	"path"
	"strings"
)

// IgnoreMatcher decides whether a corpus path is hidden from the librarian.
// The signature mirrors go-git's gitignore.Matcher.Match (a slash path split
// into components, plus whether the entry is a directory) so the Shoka adapter
// could swap in the go-git matcher behind this interface later — but the
// default implementation here is dependency-free, keeping pkg/librarian
// go-git-free (archlint TestNoGoGitImportsOutsideStorage stays green; the
// go-git gitignore parser is confined to internal/storage).
type IgnoreMatcher interface {
	// Match reports whether the path (slash components, isDir) is ignored.
	Match(path []string, isDir bool) bool
}

// NewIgnoreMatcher builds a matcher from gitignore-style patterns. ".git/" is
// always prepended as the default (the librarian never exposes the git dir);
// the product injects the rest (e.g. ".shoka*", "node_modules", "*.lock").
//
// Supported subset (what the librarian needs): basename match at any depth,
// root-anchored patterns (leading or embedded "/"), directory-only patterns
// (trailing "/"), "*"/"?"/"[...]" globs within a component, "**" spanning
// components, and "!" negation (last matching pattern wins). Blank lines and
// "#" comments are ignored.
func NewIgnoreMatcher(patterns []string) IgnoreMatcher {
	all := make([]string, 0, len(patterns)+1)
	all = append(all, ".git/")
	all = append(all, patterns...)

	m := &ignoreMatcher{}
	for _, raw := range all {
		if p, ok := parsePattern(raw); ok {
			m.patterns = append(m.patterns, p)
		}
	}
	return m
}

type ignorePattern struct {
	negate   bool
	dirOnly  bool
	anchored bool     // a leading or embedded "/" pins the pattern to the root
	segments []string // pattern split on "/" (may contain "**")
}

type ignoreMatcher struct {
	patterns []ignorePattern
}

// parsePattern parses one gitignore line. ok is false for blank/comment lines.
func parsePattern(raw string) (ignorePattern, bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return ignorePattern{}, false
	}

	var p ignorePattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	// A leading "/" anchors to the root; so does an embedded "/" (gitignore
	// rule). Drop the leading "/" so the segment list starts at the first real
	// component, but remember that the pattern is anchored.
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if line == "" {
		return ignorePattern{}, false
	}
	if strings.Contains(line, "/") {
		p.anchored = true
	}
	p.segments = strings.Split(line, "/")
	return p, true
}

// Match applies every pattern; the last one that matches the path or any of its
// ancestor directories decides (negation un-ignores).
func (m *ignoreMatcher) Match(comps []string, isDir bool) bool {
	ignored := false
	for _, p := range m.patterns {
		if p.matchesPathOrAncestor(comps, isDir) {
			ignored = !p.negate
		}
	}
	return ignored
}

// matchesPathOrAncestor reports whether the pattern fully matches the path or
// one of its ancestor directories (so a matched directory ignores everything
// beneath it).
func (p ignorePattern) matchesPathOrAncestor(comps []string, isDir bool) bool {
	for k := 1; k <= len(comps); k++ {
		isLast := k == len(comps)
		// Ancestor prefixes are directories; only the full path uses isDir.
		d := isDir || !isLast
		if p.matchesEntry(comps[:k], d) {
			return true
		}
	}
	return false
}

// matchesEntry reports whether the pattern matches the given entry exactly.
func (p ignorePattern) matchesEntry(comps []string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if !p.anchored && len(p.segments) == 1 {
		// Unanchored single segment: a basename match at any depth — compare
		// against the entry's last component.
		return fnmatch(p.segments[0], comps[len(comps)-1])
	}
	// Anchored (leading or embedded "/"): match from the root.
	return matchSegments(p.segments, comps)
}

// matchSegments reports whether the segment pattern fully consumes comps,
// honouring "**" (which spans zero or more components).
func matchSegments(pat, comps []string) bool {
	if len(pat) == 0 {
		return len(comps) == 0
	}
	if pat[0] == "**" {
		// Consume zero components, or one and recurse on the same "**".
		if matchSegments(pat[1:], comps) {
			return true
		}
		return len(comps) > 0 && matchSegments(pat, comps[1:])
	}
	if len(comps) == 0 {
		return false
	}
	if !fnmatch(pat[0], comps[0]) {
		return false
	}
	return matchSegments(pat[1:], comps[1:])
}

// fnmatch matches a single path component against a glob ("*", "?", "[...]").
// Components never contain "/", so stdlib path.Match (which treats "/"
// specially) is exact here. A malformed pattern simply never matches.
func fnmatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

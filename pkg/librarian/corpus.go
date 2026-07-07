package librarian

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Hit is one search result. Offset is the 0-based line of the first match,
// usable directly as the read tool's `offset` so the next read is ranged to the
// passage instead of pulling the whole file — the granularity fix that keeps a
// huge single file (e.g. the ~368k backlog) out of the librarian's context.
type Hit struct {
	Path    string
	Snippet string
	Offset  int
}

// Entry is one directory entry from a List.
type Entry struct {
	Name  string
	IsDir bool
}

// Corpus is the injected data source the librarian reads through. The harness
// wraps EVERY call with the guard (root-confinement + symlink-skip + ignore),
// so a Corpus implementation only does raw access — it is trusted-but-verified:
// even if Search surfaced an ignored or escaping path, the guard drops it before
// the LLM sees it, and Read/List paths are guard-validated first.
//
// Read returns only the [offset, offset+limit) line span (limit <= 0 means to
// end) — the bound that keeps a large file out of the librarian's context.
type Corpus interface {
	Search(ctx context.Context, query string, limit int) ([]Hit, error)
	Read(ctx context.Context, path string, offset, limit int) ([]byte, error)
	List(ctx context.Context, dir string) ([]Entry, error)
}

// SliceLines returns the [offset, offset+limit) slice of content's lines. A
// negative offset is clamped to 0; limit <= 0 means "to the end". Exported so a
// product's Corpus adapter (e.g. the Shoka adapter) ranges identically to the
// built-in dirCorpus.
func SliceLines(content string, offset, limit int) string {
	if offset <= 0 && limit <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], "\n")
}

const snippetRunes = 100

// snippetAround returns up to snippetRunes runes of context on each side of the
// match at byte index bidx (rune-aware, so multibyte content is never split).
func snippetAround(content string, bidx, matchByteLen int) string {
	if bidx < 0 {
		return ""
	}
	ridx := len([]rune(content[:bidx]))
	qLen := len([]rune(content[bidx : bidx+matchByteLen]))
	runes := []rune(content)
	start := ridx - snippetRunes
	if start < 0 {
		start = 0
	}
	end := ridx + qLen + snippetRunes
	if end > len(runes) {
		end = len(runes)
	}
	return strings.TrimSpace(string(runes[start:end]))
}

// dirCorpus is the built-in filesystem Corpus: a trivial substring grep + plain
// file read/list under a root. It backs the fixture-based tests and the local
// debug path; the Shoka data source (index fast-path, ranged storage read) is a
// separate product-side adapter injected via Request.Corpus.
type dirCorpus struct{ root string }

// NewDirCorpus returns a filesystem Corpus rooted at root.
func NewDirCorpus(root string) Corpus {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &dirCorpus{root: root}
}

func (c *dirCorpus) Read(_ context.Context, path string, offset, limit int) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(c.root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	return []byte(SliceLines(string(data), offset, limit)), nil
}

func (c *dirCorpus) List(_ context.Context, dir string) ([]Entry, error) {
	entries, err := os.ReadDir(filepath.Join(c.root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, Entry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

func (c *dirCorpus) Search(_ context.Context, query string, limit int) ([]Hit, error) {
	q := strings.ToLower(query)
	if q == "" {
		return nil, nil
	}

	words := strings.Fields(q)

	var hits []Hit
	err := filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		content := string(data)
		lower := strings.ToLower(content)

		// Try exact substring match first.
		bidx := strings.Index(lower, q)
		if bidx < 0 {
			// Fall back to all-words match: every word in the query
			// must appear somewhere in the file. Long words are
			// prefix-matched (min 5 chars) to handle inflection
			// differences (e.g. "documentation" matches "documented").
			if len(words) < 2 {
				return nil
			}
			for _, w := range words {
				if !containsWordOrPrefix(lower, w) {
					return nil
				}
			}
			bidx = strings.Index(lower, words[0])
		}

		rel, relErr := filepath.Rel(c.root, p)
		if relErr != nil {
			return nil
		}
		hits = append(hits, Hit{
			Path:    filepath.ToSlash(rel),
			Snippet: snippetAround(content, bidx, len(query)),
			Offset:  strings.Count(content[:bidx], "\n"),
		})
		if limit > 0 && len(hits) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

const wordPrefixMinLen = 5

func containsWordOrPrefix(content, word string) bool {
	if strings.Contains(content, word) {
		return true
	}
	if len(word) > wordPrefixMinLen {
		return strings.Contains(content, word[:wordPrefixMinLen])
	}
	return false
}

package storage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/sopranoworks/shoka/internal/storage/index"
)

// FileChange describes a file modified since a point in time/history.
type FileChange struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Kind string `json:"kind"` // added | modified | deleted
}

// SearchMatch is a file matching a query, with an optional context snippet.
// Offset is the 0-based line of the first content match (0 for filename-only
// matches); it lets a passage-oriented reader (e.g. the librarian's ranged read,
// B-73) seek to the hit instead of reading the whole file.
type SearchMatch struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet,omitempty"`
	Offset  int    `json:"offset,omitempty"`
}

const snippetRunes = 100

// ListFilesSince returns files changed after the given point, which may be an
// RFC3339 timestamp or a Git commit hash (exclusive). Each file is reported once
// with the kind of its most recent change and the hash of that commit.
func (s *FSGitStorage) ListFilesSince(namespace, projectName, since string) ([]FileChange, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}

	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	head, err := r.Head()
	if err != nil {
		return []FileChange{}, nil // no commits yet
	}

	var (
		sinceHash plumbing.Hash
		sinceTime time.Time
		hashMode  bool
		timeMode  bool
	)
	if since != "" {
		h := plumbing.NewHash(since)
		if _, e := r.CommitObject(h); e == nil {
			sinceHash, hashMode = h, true
		} else if t, e2 := time.Parse(time.RFC3339, since); e2 == nil {
			sinceTime, timeMode = t, true
		} else {
			return nil, fmt.Errorf("invalid 'since': must be a commit hash or RFC3339 timestamp")
		}
	}

	cIter, err := r.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("failed to read git log: %w", err)
	}
	defer cIter.Close()

	seen := make(map[string]bool)
	changes := []FileChange{}

	err = cIter.ForEach(func(c *object.Commit) error {
		if hashMode && c.Hash == sinceHash {
			return storer.ErrStop
		}
		if timeMode && !c.Author.When.After(sinceTime) {
			return storer.ErrStop
		}

		cTree, err := c.Tree()
		if err != nil {
			return nil
		}

		record := func(path, kind string) {
			if path == "" || seen[path] {
				return
			}
			seen[path] = true
			changes = append(changes, FileChange{Path: path, Hash: c.Hash.String(), Kind: kind})
		}

		if c.NumParents() == 0 {
			// Root commit: every file is an addition.
			fIter := cTree.Files()
			_ = fIter.ForEach(func(f *object.File) error {
				record(f.Name, "added")
				return nil
			})
			return nil
		}

		parent, err := c.Parent(0)
		if err != nil {
			return nil
		}
		parentTree, err := parent.Tree()
		if err != nil {
			return nil
		}
		diff, err := parentTree.Diff(cTree)
		if err != nil {
			return nil
		}
		for _, ch := range diff {
			action, err := ch.Action()
			if err != nil {
				continue
			}
			switch action {
			case merkletrie.Insert:
				record(ch.To.Name, "added")
			case merkletrie.Delete:
				record(ch.From.Name, "deleted")
			default:
				record(ch.To.Name, "modified")
			}
		}
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return nil, fmt.Errorf("failed to walk commits: %w", err)
	}

	return changes, nil
}

// SearchFiles returns files whose name and/or content contain query
// (case-insensitive substring). searchIn is one of "filename", "content", or
// "both" (default). Content matches include a short context snippet.
func (s *FSGitStorage) SearchFiles(namespace, projectName, query, searchIn string) ([]SearchMatch, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if searchIn == "" {
		searchIn = "both"
	}
	if searchIn != "filename" && searchIn != "content" && searchIn != "both" {
		return nil, fmt.Errorf("invalid search_in: must be filename, content, or both")
	}

	q := strings.ToLower(query)
	matches := []SearchMatch{}

	// Full-text fast path (I2, decision A — gated-walk): when content is searched,
	// the query has at least one bigram, and the project's derivative index is
	// healthy, the index narrows which files have their content read. The walk,
	// filename matching, order, and makeSnippet are the SAME code path as the
	// fallback, so the result is byte-for-byte identical — only files whose indexed
	// bigram set cannot contain the query skip the os.ReadFile. A file with no index
	// record (unindexed / a failed best-effort update) is always read, so the gate
	// never causes a false negative for a tracked file; truth-verify (the substring
	// check below) removes every bigram false positive. A query shorter than 2 runes
	// (no bigram) or an unhealthy/absent index falls through to reading every file.
	searchesContent := searchIn == "content" || searchIn == "both"
	var (
		queryBigrams []string
		ix           *index.Index
	)
	if searchesContent {
		queryBigrams = index.Bigrams(query)
		if len(queryBigrams) > 0 && s.IndexHealthy(namespace, projectName) {
			ix = s.indexForRead(namespace, projectName)
		}
		// One atomic add per content query (M2): fastpath when the index engaged
		// (narrowing reads), fallback when no query bigram or an unhealthy/absent
		// index means every file is read. Filename-only searches never reach here,
		// so they are counted in neither bucket. This is the only metric touch on
		// the search path and it is once per query, never per file.
		if ix != nil {
			s.searchFastpath.Add(1)
		} else {
			s.searchFallback.Add(1)
		}
	}

	walkErr := filepath.WalkDir(projectPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != projectPath && derivativeWalkSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if derivativeWalkSkipFile(d.Name()) {
			return nil
		}

		rel, relErr := filepath.Rel(projectPath, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		nameMatch := strings.Contains(strings.ToLower(rel), q)

		contentMatch := false
		snippet := ""
		matchLine := 0
		if searchesContent {
			// Gate the content read on the index: skip only when it definitively
			// knows this file cannot contain the query (record present AND its
			// bigram set lacks one of the query's). Absent record / read error →
			// read (safe). This is the only place the fast path diverges from the
			// fallback's work, never from its result.
			read := true
			if ix != nil {
				if rec, found, gerr := ix.GetRecord(rel); gerr == nil && found && !rec.ContainsAllBigrams(queryBigrams) {
					read = false
				}
			}
			if read {
				data, readErr := os.ReadFile(p)
				if readErr == nil && strings.Contains(strings.ToLower(string(data)), q) {
					contentMatch = true
					snippet, matchLine = makeSnippet(string(data), q)
				}
			}
		}

		switch searchIn {
		case "filename":
			if nameMatch {
				matches = append(matches, SearchMatch{Path: rel})
			}
		case "content":
			if contentMatch {
				matches = append(matches, SearchMatch{Path: rel, Snippet: snippet, Offset: matchLine})
			}
		default: // both
			if nameMatch || contentMatch {
				matches = append(matches, SearchMatch{Path: rel, Snippet: snippet, Offset: matchLine})
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to search files: %w", walkErr)
	}

	return matches, nil
}

// SearchFastpathStats returns the cumulative count of content-searching queries
// that engaged the index fast path versus those that fell back to reading every
// file, for shoka_search_fastpath_total{outcome}. It counts once per content query
// (filename-only searches reach neither bucket). It tells the present->fast /
// absent->slow story: a healthy, populated index pushes the ratio toward fastpath.
func (s *FSGitStorage) SearchFastpathStats() (fastpath, fallback int64) {
	return s.searchFastpath.Load(), s.searchFallback.Load()
}

// makeSnippet returns up to snippetRunes runes of context on each side of the
// first (case-insensitive) occurrence of queryLower in content, plus the 0-based
// line number of that occurrence. It is rune-aware so multibyte content (e.g.
// Japanese) is never split mid-rune. A non-match returns ("", 0).
func makeSnippet(content, queryLower string) (string, int) {
	bidx := strings.Index(strings.ToLower(content), queryLower)
	if bidx < 0 {
		return "", 0
	}
	line := strings.Count(content[:bidx], "\n")
	ridx := len([]rune(content[:bidx]))
	qLen := len([]rune(content[bidx : bidx+len(queryLower)]))

	runes := []rune(content)
	start := ridx - snippetRunes
	if start < 0 {
		start = 0
	}
	end := ridx + qLen + snippetRunes
	if end > len(runes) {
		end = len(runes)
	}
	return strings.TrimSpace(string(runes[start:end])), line
}

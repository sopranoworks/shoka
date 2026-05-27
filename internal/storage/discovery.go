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
)

// FileChange describes a file modified since a point in time/history.
type FileChange struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Kind string `json:"kind"` // added | modified | deleted
}

// SearchMatch is a file matching a query, with an optional context snippet.
type SearchMatch struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet,omitempty"`
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

	walkErr := filepath.WalkDir(projectPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != projectPath && (d.Name() == ".git" || d.Name() == ".drafts") {
				return filepath.SkipDir
			}
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
		if searchIn == "content" || searchIn == "both" {
			data, readErr := os.ReadFile(p)
			if readErr == nil && strings.Contains(strings.ToLower(string(data)), q) {
				contentMatch = true
				snippet = makeSnippet(string(data), q)
			}
		}

		switch searchIn {
		case "filename":
			if nameMatch {
				matches = append(matches, SearchMatch{Path: rel})
			}
		case "content":
			if contentMatch {
				matches = append(matches, SearchMatch{Path: rel, Snippet: snippet})
			}
		default: // both
			if nameMatch || contentMatch {
				matches = append(matches, SearchMatch{Path: rel, Snippet: snippet})
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to search files: %w", walkErr)
	}

	return matches, nil
}

// makeSnippet returns up to snippetRunes runes of context on each side of the
// first (case-insensitive) occurrence of queryLower in content. It is rune-aware
// so multibyte content (e.g. Japanese) is never split mid-rune.
func makeSnippet(content, queryLower string) string {
	bidx := strings.Index(strings.ToLower(content), queryLower)
	if bidx < 0 {
		return ""
	}
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
	return strings.TrimSpace(string(runes[start:end]))
}

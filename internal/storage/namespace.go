package storage

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ListNamespaces returns every namespace that exists on disk, sorted ascending.
// Returns an empty slice when none exist.
//
// A namespace is an immediate, non-hidden subdirectory of base_dir. It is a
// FIRST-CLASS object (B-28 namespace/project management): a namespace is enumerated
// whether or not it currently holds any projects — an explicitly-created empty namespace
// (CreateNamespace) appears here, and a namespace SURVIVES the deletion of its last
// project (it no longer auto-vanishes; only DeleteNamespace removes it). Hidden
// directories (the ".shoka" WAL/state store, the ".shoka-lostfound" quarantine area)
// and regular files (the users.db / oauth.db / per-project <project>.db siblings) are
// skipped — a dot-prefixed entry is Shoka-internal, never a namespace.
func (s *FSGitStorage) ListNamespaces() ([]string, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read base directory: %w", err)
	}

	namespaces := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		namespaces = append(namespaces, entry.Name())
	}
	sort.Strings(namespaces)
	return namespaces, nil
}

// ListAllProjects returns every project across every namespace as a sorted
// slice of "<namespace>/<name>" strings. Returns an empty slice when no
// projects exist. Read-only: no lock, no git access.
func (s *FSGitStorage) ListAllProjects() ([]string, error) {
	namespaces, err := s.ListNamespaces()
	if err != nil {
		return nil, err
	}

	all := make([]string, 0)
	for _, ns := range namespaces {
		projects, err := s.ListProjects(ns)
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			all = append(all, ns+"/"+p)
		}
	}
	sort.Strings(all)
	return all, nil
}

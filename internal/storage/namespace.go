package storage

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ListNamespaces returns every namespace that currently has at least one
// project on disk. The result is sorted ascending. Returns an empty slice when
// no projects exist.
//
// A namespace is an immediate subdirectory of base_dir that contains at least
// one project subdirectory. Hidden directories (e.g. the ".shoka" WAL/state
// store) and regular files are skipped, as are namespace directories that hold
// no projects.
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
		projects, err := s.ListProjects(entry.Name())
		if err != nil {
			return nil, err
		}
		if len(projects) > 0 {
			namespaces = append(namespaces, entry.Name())
		}
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

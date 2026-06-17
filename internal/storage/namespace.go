package storage

import (
	"fmt"
	"sort"
)

// ListNamespaces returns every MANAGED namespace, sorted ascending. Returns an empty
// slice when none are managed.
//
// A namespace is a directory Shoka MANAGES, not "whatever subdirectory exists" (the
// B-28 stage-A correction of the part-1 regression, which wrongly enumerated every
// base-dir subdir). The managed set is the explicit registry (<base>/namespaces.db): a
// namespace appears here only once Shoka has taken it under management — via
// CreateNamespace, as the auto-registered parent of a CreateProject, the always-managed
// `default`, or the one-time rescue-adopt migration. A managed namespace is enumerated
// whether or not it currently holds projects, and SURVIVES the deletion of its last
// project; only DeleteNamespace removes it. A bare directory dropped into base_dir is
// NOT a namespace and is not listed (it is at most an adoptable foreign dir, handled by
// stage B's health/recovery).
func (s *FSGitStorage) ListNamespaces() ([]string, error) {
	if s.nsReg == nil {
		return []string{}, nil
	}
	names, err := s.nsReg.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list managed namespaces: %w", err)
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
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

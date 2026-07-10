package storage

import (
	"os"
	"path/filepath"
	"strings"
)

// projectEntryKind classifies a namespace-level directory entry for the two
// project-enumeration paths (discoverProjects and ListProjects).
type projectEntryKind int

const (
	// entrySkip: not a project candidate at all — a non-directory (e.g. the
	// "@<project>.project.db" catalog sibling file) or a dot-prefixed name (a Shoka-internal
	// area: .shoka-lostfound, .shoka, .drafts, .git). Enumeration skips it outright;
	// it is never a project and never a relocatable leftover.
	entrySkip projectEntryKind = iota
	// entryLeftover: a non-dot directory with no .git — not a project but a remnant
	// (a pre-B-37 guard-less write half-created one, or an external stray).
	// discoverProjects surfaces it for relocation to lost+found; ListProjects excludes
	// it from the project list.
	entryLeftover
	// entryProject: a non-dot directory containing a git repo — a real project.
	entryProject
)

// classifyProjectEntry is the SINGLE project-eligibility predicate shared by
// discoverProjects (startup) and ListProjects (the UI/MCP enumeration path), so the
// two can never disagree about what counts as a project (B-31). namespacePath is the
// absolute path of the namespace directory; entry is one of its os.ReadDir entries.
//
// The dot-prefix skip subsumes the derivative/quarantine dirs (.git/.shoka/.drafts)
// AND the lost+found area (.shoka-lostfound), matching discoverProjects's long-
// standing guard (B-26); the hasGitRepo check is what separates a real project from a
// repo-less leftover (B-37). Before B-31, ListProjects applied NEITHER and so listed
// .shoka-lostfound (and would have listed a repo-less leftover) as a phantom project.
func classifyProjectEntry(namespacePath string, entry os.DirEntry) projectEntryKind {
	if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
		return entrySkip
	}
	if !hasGitRepo(filepath.Join(namespacePath, entry.Name())) {
		return entryLeftover
	}
	return entryProject
}

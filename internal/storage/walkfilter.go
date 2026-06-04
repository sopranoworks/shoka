package storage

import "strings"

// The single source of truth for which working-tree entries are excluded from the
// derivative-index corpus AND the SearchFiles content scan. Sharing one predicate
// across workingTreeContentHashes (drift.go), workingTreeIndexRecords
// (index_sweep.go), and SearchFiles (discovery.go) keeps the index corpus and the
// fast path's fallback-scan corpus identical by construction (I2, decision A +
// finding 2) — so the bigram fast path can never miss a file the full scan would
// find, nor return one it would not.

// derivativeWalkSkipDir reports whether a directory (other than the project root)
// is excluded. .git is the repository; .shoka is the namespace-level WAL/state
// area; .drafts is the unstable-connection draft store — none are managed,
// searchable project files.
func derivativeWalkSkipDir(name string) bool {
	switch name {
	case ".git", ".shoka", ".drafts":
		return true
	}
	return false
}

// derivativeWalkSkipFile reports whether a file is excluded. .tmp-write-* are
// transient atomic-write staging files: a half-written staging file is never a
// valid search hit, and excluding it from SearchFiles (the I2 finding-2 alignment)
// keeps the fast path and the fallback identical even on that edge.
func derivativeWalkSkipFile(name string) bool {
	return strings.HasPrefix(name, ".tmp-write-")
}

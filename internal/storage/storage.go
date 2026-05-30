package storage

import (
	"context"
	"time"
)

// CommitInfo represents Git commit metadata.
type CommitInfo struct {
	Hash    string    `json:"hash"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
	Message string    `json:"message"`
}

// StorageService defines the interface for project and file management with Git support.
type StorageService interface {
	// CreateProject initializes a new project directory and a Git repository within it.
	CreateProject(namespace, projectName string) error

	// WriteFile writes content to a file in a project and performs an atomic Git commit.
	WriteFile(namespace, projectName, path, content string) error

	// ReadFile reads the content of a file from a project.
	ReadFile(namespace, projectName, path string) (string, error)

	// ReadFileWithETag reads a file and returns its content and etag (the
	// SHA-256 of the content). No lock, no git access.
	ReadFileWithETag(namespace, projectName, path string) (string, string, error)

	// StatModTime returns a single file's working-tree filesystem mtime
	// (os.Stat().ModTime()) — the same inode mtime ListFiles reports. No lock,
	// no git access; reflects the latest write immediately.
	StatModTime(namespace, projectName, path string) (time.Time, error)

	// Write writes content with optimistic concurrency. ifMatch nil skips the
	// check; non-nil requires the current etag to equal *ifMatch (a
	// *VersionConflictError is returned otherwise). Returns the new etag.
	Write(ctx context.Context, sessionID, namespace, projectName, path, content string, ifMatch *string) (string, error)

	// Delete removes a file with optimistic concurrency (see Write).
	Delete(ctx context.Context, sessionID, namespace, projectName, path string, ifMatch *string) error

	// ListProjects returns a list of project names within a namespace.
	ListProjects(namespace string) ([]string, error)

	// ListNamespaces returns every namespace with at least one project on disk,
	// sorted ascending.
	ListNamespaces() ([]string, error)

	// ListAllProjects returns every project across every namespace as a sorted
	// slice of "<namespace>/<name>" strings.
	ListAllProjects() ([]string, error)

	// New methods for Phase 2

	// DeleteFile deletes a file from a project and performs an atomic Git commit.
	DeleteFile(namespace, projectName, path string) error

	// ListFiles returns the non-recursive listing of a project path: entry
	// names (directories carry a trailing "/") and a parallel map of each
	// entry's working-tree modification time, keyed by the same display name.
	ListFiles(namespace, projectName, path string) ([]string, map[string]time.Time, error)

	// GetHistory returns the commit history for a specific file.
	GetHistory(namespace, projectName, path string, limit int) ([]CommitInfo, error)

	// ReadFileAtVersion reads the content of a file at a specific Git commit hash.
	ReadFileAtVersion(namespace, projectName, path, hash string) (string, error)

	// New methods for Phase 3 (optimistic concurrency)

	// GetCurrentVersion returns the hash of the most recent commit that modified
	// the file at path, or "" if the file has no commit history.
	GetCurrentVersion(namespace, projectName, path string) (string, error)

	// WriteFileVersioned writes with optimistic locking. When expectedVersion is
	// non-empty it must match the file's current version or a *VersionConflictError
	// is returned. It returns the hash of the new commit.
	WriteFileVersioned(namespace, projectName, path, content, expectedVersion string) (string, error)

	// DeleteFileVersioned deletes with optimistic locking (see WriteFileVersioned),
	// returning the hash of the new commit.
	DeleteFileVersioned(namespace, projectName, path, expectedVersion string) (string, error)

	// New methods for Phase 5 (change detection & discovery)

	// ListFilesSince returns files changed after the given point (an RFC3339
	// timestamp or a commit hash, exclusive), each with its change kind.
	ListFilesSince(namespace, projectName, since string) ([]FileChange, error)

	// SearchFiles returns files matching query by filename, content, or both.
	SearchFiles(namespace, projectName, query, searchIn string) ([]SearchMatch, error)
}

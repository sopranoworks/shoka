package storage

import "time"

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

	// ListProjects returns a list of project names within a namespace.
	ListProjects(namespace string) ([]string, error)

	// New methods for Phase 2

	// DeleteFile deletes a file from a project and performs an atomic Git commit.
	DeleteFile(namespace, projectName, path string) error

	// ListFiles returns a list of files in a project path.
	ListFiles(namespace, projectName, path string) ([]string, error)

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
}

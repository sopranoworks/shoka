package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoka/mcp-server/internal/utils"
)

// FSGitStorage implements StorageService using the local filesystem and Git.
type FSGitStorage struct {
	baseDir string
	// mu serializes writes/deletes so the optimistic-locking check-and-commit is
	// atomic and concurrent go-git operations don't race on the worktree index.
	mu            sync.Mutex
	changeHandler ChangeHandler
}

// ChangeEvent describes a successful mutation, delivered to a registered handler.
type ChangeEvent struct {
	Event      string // file_written | file_deleted | project_created
	Namespace  string
	Project    string
	Path       string
	CommitHash string
	Timestamp  time.Time
}

// ChangeHandler receives ChangeEvents after a successful mutation. It must not
// block (e.g. it should fan out to webhooks asynchronously).
type ChangeHandler func(ChangeEvent)

// SetChangeHandler registers a handler invoked after successful writes, deletes,
// and project creation, covering every write path (MCP tools and the web UI).
func (s *FSGitStorage) SetChangeHandler(h ChangeHandler) {
	s.changeHandler = h
}

func (s *FSGitStorage) emit(ev ChangeEvent) {
	if s.changeHandler != nil {
		s.changeHandler(ev)
	}
}

// VersionConflictError is returned by versioned writes/deletes when the caller's
// expected version does not match the file's current version.
type VersionConflictError struct {
	Expected string
	Current  string
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version conflict: expected %q but current version is %q", e.Expected, e.Current)
}

// NewFSGitStorage creates a new FSGitStorage instance.
func NewFSGitStorage(baseDir string) (*FSGitStorage, error) {
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for base directory: %w", err)
	}

	if err := os.MkdirAll(absBaseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &FSGitStorage{baseDir: absBaseDir}, nil
}

func (s *FSGitStorage) getProjectPath(namespace, projectName string) (string, error) {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return "", fmt.Errorf("invalid namespace: %s", namespace)
	}
	if !utils.IsValidName(projectName) {
		return "", fmt.Errorf("invalid project name: %s", projectName)
	}
	return filepath.Join(s.baseDir, namespace, projectName), nil
}

// CreateProject initializes a new project directory and a Git repository within it.
func (s *FSGitStorage) CreateProject(namespace, projectName string) error {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(projectPath, 0755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}

	_, err = git.PlainInit(projectPath, false)
	if err != nil {
		if err == git.ErrRepositoryAlreadyExists {
			return nil
		}
		return fmt.Errorf("failed to initialize git repository: %w", err)
	}

	s.emit(ChangeEvent{
		Event:     "project_created",
		Namespace: namespace,
		Project:   projectName,
		Timestamp: time.Now(),
	})

	return nil
}

// WriteFile writes content to a file in a project and performs an atomic Git commit.
func (s *FSGitStorage) WriteFile(namespace, projectName, path, content string) error {
	_, err := s.writeFile(namespace, projectName, path, content, "")
	return err
}

// WriteFileVersioned writes content with optimistic locking. When expectedVersion
// is non-empty it must equal the file's current version (the hash of the most
// recent commit touching the file); otherwise a *VersionConflictError is returned
// and no write occurs. It returns the hash of the new commit.
func (s *FSGitStorage) WriteFileVersioned(namespace, projectName, path, content, expectedVersion string) (string, error) {
	return s.writeFile(namespace, projectName, path, content, expectedVersion)
}

func (s *FSGitStorage) writeFile(namespace, projectName, path, content, expectedVersion string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(projectPath, path)

	// Robust path traversal protection
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid file path: %s", path)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if expectedVersion != "" {
		current, err := s.currentVersion(projectPath, rel)
		if err != nil {
			return "", err
		}
		if current != expectedVersion {
			return "", &VersionConflictError{Expected: expectedVersion, Current: current}
		}
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create directories for file: %w", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	// Git commit
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}

	w, err := r.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree: %w", err)
	}

	if _, err := w.Add(rel); err != nil { // Use relative path for git add
		return "", fmt.Errorf("failed to add file to git: %w", err)
	}

	hash, err := w.Commit(fmt.Sprintf("Update %s", rel), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "MCP Server",
			Email: "mcp-server@shoka.io",
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to commit changes: %w", err)
	}

	s.emit(ChangeEvent{
		Event:      "file_written",
		Namespace:  namespace,
		Project:    projectName,
		Path:       path,
		CommitHash: hash.String(),
		Timestamp:  time.Now(),
	})

	return hash.String(), nil
}

// ReadFile reads the content of a file from a project.
func (s *FSGitStorage) ReadFile(namespace, projectName, path string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(projectPath, path)

	// Robust path traversal protection
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid file path: %s", path)
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	return string(content), nil
}

// ListProjects returns a list of project names within a namespace.
func (s *FSGitStorage) ListProjects(namespace string) ([]string, error) {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return nil, fmt.Errorf("invalid namespace: %s", namespace)
	}
	namespacePath := filepath.Join(s.baseDir, namespace)

	entries, err := os.ReadDir(namespacePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read namespace directory: %w", err)
	}

	var projects []string
	for _, entry := range entries {
		if entry.IsDir() {
			projects = append(projects, entry.Name())
		}
	}

	return projects, nil
}

// DeleteFile deletes a file from a project and performs an atomic Git commit.
func (s *FSGitStorage) DeleteFile(namespace, projectName, path string) error {
	_, err := s.deleteFile(namespace, projectName, path, "")
	return err
}

// DeleteFileVersioned deletes a file with optimistic locking (see WriteFileVersioned),
// returning the hash of the new (delete) commit.
func (s *FSGitStorage) DeleteFileVersioned(namespace, projectName, path, expectedVersion string) (string, error) {
	return s.deleteFile(namespace, projectName, path, expectedVersion)
}

func (s *FSGitStorage) deleteFile(namespace, projectName, path, expectedVersion string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(projectPath, path)

	// Robust path traversal protection
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid file path: %s", path)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if expectedVersion != "" {
		current, err := s.currentVersion(projectPath, rel)
		if err != nil {
			return "", err
		}
		if current != expectedVersion {
			return "", &VersionConflictError{Expected: expectedVersion, Current: current}
		}
	}

	// Remove from disk
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to remove file from disk: %w", err)
	}

	// Git commit
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}

	w, err := r.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree: %w", err)
	}

	if _, err := w.Remove(rel); err != nil {
		return "", fmt.Errorf("failed to remove file from git: %w", err)
	}

	hash, err := w.Commit(fmt.Sprintf("Delete %s", rel), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "MCP Server",
			Email: "mcp-server@shoka.io",
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to commit changes: %w", err)
	}

	s.emit(ChangeEvent{
		Event:      "file_deleted",
		Namespace:  namespace,
		Project:    projectName,
		Path:       path,
		CommitHash: hash.String(),
		Timestamp:  time.Now(),
	})

	return hash.String(), nil
}

// ListFiles returns a list of files in a project path (non-recursive).
func (s *FSGitStorage) ListFiles(namespace, projectName, path string) ([]string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}

	searchPath := filepath.Join(projectPath, path)

	// Robust path traversal protection
	relSearch, err := filepath.Rel(projectPath, searchPath)
	if err != nil || (relSearch != "." && (strings.HasPrefix(relSearch, "..") || strings.HasPrefix(relSearch, "/"))) {
		return nil, fmt.Errorf("invalid search path: %s", path)
	}

	entries, err := os.ReadDir(searchPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || name == ".drafts" {
			continue
		}
		if entry.IsDir() {
			files = append(files, name+"/")
		} else {
			files = append(files, name)
		}
	}

	return files, nil
}

// GetHistory returns the commit history for a specific file.
func (s *FSGitStorage) GetHistory(namespace, projectName, path string, limit int) ([]CommitInfo, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}

	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	logOptions := &git.LogOptions{Order: git.LogOrderCommitterTime}
	if path != "" {
		// Ensure path is relative and clean
		fullPath := filepath.Join(projectPath, path)
		rel, err := filepath.Rel(projectPath, fullPath)
		if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
			return nil, fmt.Errorf("invalid file path: %s", path)
		}
		logOptions.FileName = &rel
	}

	cIter, err := r.Log(logOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to get git log: %w", err)
	}
	defer cIter.Close()

	var history []CommitInfo
	for {
		if limit > 0 && len(history) >= limit {
			break
		}

		c, err := cIter.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to get next commit: %w", err)
		}

		history = append(history, CommitInfo{
			Hash:    c.Hash.String(),
			Author:  c.Author.Name,
			Date:    c.Author.When,
			Message: c.Message,
		})
	}

	return history, nil
}

// ReadFileAtVersion reads the content of a file at a specific Git commit hash.
func (s *FSGitStorage) ReadFileAtVersion(namespace, projectName, path, hash string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}

	// Robust path traversal protection
	fullPath := filepath.Join(projectPath, path)
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid file path: %s", path)
	}

	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}

	h := plumbing.NewHash(hash)
	commit, err := r.CommitObject(h)
	if err != nil {
		return "", fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("failed to get tree: %w", err)
	}

	file, err := tree.File(rel)
	if err != nil {
		return "", fmt.Errorf("failed to get file from tree: %w", err)
	}

	content, err := file.Contents()
	if err != nil {
		return "", fmt.Errorf("failed to read file contents: %w", err)
	}

	return content, nil
}

// GetCurrentVersion returns the hash of the most recent commit that modified the
// file at path, or "" if the file has no commit history.
func (s *FSGitStorage) GetCurrentVersion(namespace, projectName, path string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(projectPath, path)
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid file path: %s", path)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentVersion(projectPath, rel)
}

// currentVersion returns the latest commit hash touching rel within projectPath.
// The caller must hold s.mu.
func (s *FSGitStorage) currentVersion(projectPath, rel string) (string, error) {
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}
	if _, err := r.Head(); err != nil {
		// No commits yet.
		return "", nil
	}
	cIter, err := r.Log(&git.LogOptions{Order: git.LogOrderCommitterTime, FileName: &rel})
	if err != nil {
		return "", fmt.Errorf("failed to read git log: %w", err)
	}
	defer cIter.Close()

	c, err := cIter.Next()
	if err == io.EOF {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read commit: %w", err)
	}
	return c.Hash.String(), nil
}

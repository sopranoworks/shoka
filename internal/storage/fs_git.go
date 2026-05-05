package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoka/mcp-server/internal/utils"
)

// FSGitStorage implements StorageService using the local filesystem and Git.
type FSGitStorage struct {
	baseDir string
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

	return nil
}

// WriteFile writes content to a file in a project and performs an atomic Git commit.
func (s *FSGitStorage) WriteFile(namespace, projectName, path, content string) error {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	fullPath := filepath.Join(projectPath, path)

	// Robust path traversal protection
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("invalid file path: %s", path)
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("failed to create directories for file: %w", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Git commit
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	_, err = w.Add(rel) // Use relative path for git add
	if err != nil {
		return fmt.Errorf("failed to add file to git: %w", err)
	}

	_, err = w.Commit(fmt.Sprintf("Update %s", rel), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "MCP Server",
			Email: "mcp-server@shoka.io",
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to commit changes: %w", err)
	}

	return nil
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
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	fullPath := filepath.Join(projectPath, path)

	// Robust path traversal protection
	rel, err := filepath.Rel(projectPath, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("invalid file path: %s", path)
	}

	// Remove from disk
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file from disk: %w", err)
	}

	// Git commit
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	_, err = w.Remove(rel)
	if err != nil {
		return fmt.Errorf("failed to remove file from git: %w", err)
	}

	_, err = w.Commit(fmt.Sprintf("Delete %s", rel), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "MCP Server",
			Email: "mcp-server@shoka.io",
			When:  time.Now(),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to commit changes: %w", err)
	}

	return nil
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
		if name == ".git" {
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

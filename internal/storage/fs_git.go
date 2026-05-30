package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/shoka/mcp-server/internal/storage/filelock"
	"github.com/shoka/mcp-server/internal/storage/wal"
	"github.com/shoka/mcp-server/internal/storage/walworker"
	"github.com/shoka/mcp-server/internal/utils"
)

// FSGitStorage implements StorageService with the file system as the ground
// truth and git as a background audit log (the 2026-05-30 storage redesign).
//
// Reads are pure os.ReadFile — no lock, no git. Writes take a per-file lock only
// across the check-and-write critical section, append to a write-ahead log, and
// return immediately; the git commit happens later in a background worker pool.
// The old global s.mu is gone.
type FSGitStorage struct {
	baseDir string

	locks         *filelock.Manager
	wal           *wal.Log
	pool          *walworker.Pool
	maxWALEntries int

	stateMu sync.RWMutex
	states  map[string]ProjectState

	changeHandler ChangeHandler
	logger        *slog.Logger
}

// Options carries the storage-redesign tunables (§12). Zero values take the
// component packages' defaults.
type Options struct {
	FileLock      filelock.Config
	WALMaxEntries int
	WALWorker     walworker.Config
}

// ChangeEvent describes a successful mutation, delivered to a registered handler
// after the background git commit lands.
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

// Typed errors returned by the write path. Callers (and the MCP tool layer in
// §8) distinguish them to produce the structured {reason} responses.
var (
	// ErrProjectDangerous means the project's .git is unreadable/absent.
	ErrProjectDangerous = errors.New("project is in dangerous state: git repository is unreadable")
	// ErrProjectCorrupted means the working tree drifted from git HEAD outside
	// the redesign's write path (hand-edit, git pull, another tool).
	ErrProjectCorrupted = errors.New("project is in corrupted state: working tree has uncommitted drift")
	// ErrWriteDisabled means the WAL has backed up past its threshold.
	ErrWriteDisabled = errors.New("writes are disabled: write-ahead log is full")
)

// VersionConflictError is returned by writes/deletes when the caller's expected
// value (if_match) does not match the file's current etag. Current is the
// current etag (sha256 of the file's content right now).
type VersionConflictError struct {
	Expected string
	Current  string
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("etag conflict: expected %q but current etag is %q", e.Expected, e.Current)
}

// SetChangeHandler registers a handler invoked after successful writes, deletes,
// and project creation.
func (s *FSGitStorage) SetChangeHandler(h ChangeHandler) { s.changeHandler = h }

// SetLogger attaches a structured logger.
func (s *FSGitStorage) SetLogger(l *slog.Logger) { s.logger = l }

func (s *FSGitStorage) log() *slog.Logger {
	if s.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.logger
}

func (s *FSGitStorage) emit(ev ChangeEvent) {
	s.log().Info("git change committed",
		"event", ev.Event,
		"namespace", ev.Namespace,
		"project", ev.Project,
		"path", ev.Path,
		"commit", ev.CommitHash,
	)
	if s.changeHandler != nil {
		s.changeHandler(ev)
	}
}

// NewFSGitStorage creates storage with default tunables.
func NewFSGitStorage(baseDir string) (*FSGitStorage, error) {
	return NewFSGitStorageWithOptions(baseDir, Options{})
}

// NewFSGitStorageWithOptions creates storage and starts the lock reaper and the
// background commit worker pool. Any entries already in the WAL (from a previous
// run) are drained on startup.
func NewFSGitStorageWithOptions(baseDir string, opts Options) (*FSGitStorage, error) {
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for base directory: %w", err)
	}
	if err := os.MkdirAll(absBaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	w, err := wal.Open(absBaseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL: %w", err)
	}

	maxWAL := opts.WALMaxEntries
	if maxWAL <= 0 {
		maxWAL = 1000
	}

	s := &FSGitStorage{
		baseDir:       absBaseDir,
		locks:         filelock.NewManager(opts.FileLock),
		wal:           w,
		maxWALEntries: maxWAL,
		states:        make(map[string]ProjectState),
	}
	s.pool = walworker.NewPool(w, s.commitEntry, opts.WALWorker)
	return s, nil
}

// Close stops the worker pool and lock reaper. WAL files on disk are preserved.
func (s *FSGitStorage) Close() error {
	if s.pool != nil {
		_ = s.pool.Shutdown(30 * time.Second)
	}
	if s.locks != nil {
		s.locks.Stop()
	}
	if s.wal != nil {
		_ = s.wal.Close()
	}
	return nil
}

func projectKey(namespace, projectName string) string { return namespace + "/" + projectName }

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

// relWithin validates path traversal and returns the slash-relative path.
func relWithin(projectPath, path string) (string, string, error) {
	fullPath := filepath.Join(projectPath, path)
	rel, err := filepath.Rel(projectPath, fullPath)
	if filepath.IsAbs(path) || err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", "", fmt.Errorf("invalid file path: %s", path)
	}
	return fullPath, filepath.ToSlash(rel), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateProject initializes a new project directory and a Git repository within it.
func (s *FSGitStorage) CreateProject(namespace, projectName string) error {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}
	_, err = git.PlainInit(projectPath, false)
	if err != nil {
		if err == git.ErrRepositoryAlreadyExists {
			return nil
		}
		return fmt.Errorf("failed to initialize git repository: %w", err)
	}
	s.setState(namespace, projectName, StateHealthy)
	s.emit(ChangeEvent{
		Event:     "project_created",
		Namespace: namespace,
		Project:   projectName,
		Timestamp: time.Now(),
	})
	return nil
}

// WriteFile writes content with no optimistic-concurrency check.
func (s *FSGitStorage) WriteFile(namespace, projectName, path, content string) error {
	_, err := s.write(context.Background(), "", namespace, projectName, path, content, nil)
	return err
}

// WriteFileVersioned writes with optimistic locking. A non-empty expectedVersion
// must equal the file's current etag (sha256 of its content) or a
// *VersionConflictError is returned. Returns the new etag.
func (s *FSGitStorage) WriteFileVersioned(namespace, projectName, path, content, expectedVersion string) (string, error) {
	return s.write(context.Background(), "", namespace, projectName, path, content, ifMatchPtr(expectedVersion))
}

// Write is the redesign's write entry point (used by the §8 tool layer). ifMatch
// nil skips the check; non-nil requires the current etag to equal *ifMatch.
func (s *FSGitStorage) Write(ctx context.Context, sessionID, namespace, projectName, path, content string, ifMatch *string) (string, error) {
	return s.write(ctx, sessionID, namespace, projectName, path, content, ifMatch)
}

func ifMatchPtr(expected string) *string {
	if expected == "" {
		return nil
	}
	return &expected
}

func (s *FSGitStorage) write(ctx context.Context, sessionID, namespace, projectName, path, content string, ifMatch *string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	if err := s.checkWritable(namespace, projectName); err != nil {
		return "", err
	}

	var newEtag string
	lockErr := s.locks.WithLock(ctx, sessionID, fullPath, func() error {
		current, _ := os.ReadFile(fullPath) // empty if absent
		currentEtag := sha256Hex(current)
		if ifMatch != nil && *ifMatch != currentEtag {
			return &VersionConflictError{Expected: *ifMatch, Current: currentEtag}
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("failed to create directories for file: %w", err)
		}
		if err := atomicWriteFile(fullPath, []byte(content)); err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}
		newEtag = sha256Hex([]byte(content))
		if _, err := s.wal.Append(wal.Entry{
			Namespace: namespace,
			Project:   projectName,
			Path:      rel,
			Op:        "write",
			Content:   []byte(content),
		}); err != nil {
			return fmt.Errorf("failed to append to WAL: %w", err)
		}
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	s.pool.Notify()
	return newEtag, nil
}

// ReadFile reads the content of a file from a project. No lock, no git access.
func (s *FSGitStorage) ReadFile(namespace, projectName, path string) (string, error) {
	content, _, err := s.ReadFileWithETag(namespace, projectName, path)
	return content, err
}

// ReadFileWithETag reads a file and returns its content and etag (sha256 of the
// content). It takes no lock and never touches git. Dangerous projects are
// refused.
func (s *FSGitStorage) ReadFileWithETag(namespace, projectName, path string) (string, string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", "", err
	}
	if s.State(namespace, projectName) == StateDangerous {
		return "", "", ErrProjectDangerous
	}
	fullPath, _, err := relWithin(projectPath, path)
	if err != nil {
		return "", "", err
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read file: %w", err)
	}
	return string(content), sha256Hex(content), nil
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

// DeleteFile deletes a file with no optimistic-concurrency check.
func (s *FSGitStorage) DeleteFile(namespace, projectName, path string) error {
	_, err := s.deleteFile(context.Background(), "", namespace, projectName, path, nil)
	return err
}

// DeleteFileVersioned deletes with optimistic locking (see WriteFileVersioned).
func (s *FSGitStorage) DeleteFileVersioned(namespace, projectName, path, expectedVersion string) (string, error) {
	return s.deleteFile(context.Background(), "", namespace, projectName, path, ifMatchPtr(expectedVersion))
}

// Delete is the redesign's delete entry point (used by the §8 tool layer).
func (s *FSGitStorage) Delete(ctx context.Context, sessionID, namespace, projectName, path string, ifMatch *string) error {
	_, err := s.deleteFile(ctx, sessionID, namespace, projectName, path, ifMatch)
	return err
}

func (s *FSGitStorage) deleteFile(ctx context.Context, sessionID, namespace, projectName, path string, ifMatch *string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	if err := s.checkWritable(namespace, projectName); err != nil {
		return "", err
	}

	lockErr := s.locks.WithLock(ctx, sessionID, fullPath, func() error {
		current, _ := os.ReadFile(fullPath)
		currentEtag := sha256Hex(current)
		if ifMatch != nil && *ifMatch != currentEtag {
			return &VersionConflictError{Expected: *ifMatch, Current: currentEtag}
		}
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file from disk: %w", err)
		}
		if _, err := s.wal.Append(wal.Entry{
			Namespace: namespace,
			Project:   projectName,
			Path:      rel,
			Op:        "delete",
		}); err != nil {
			return fmt.Errorf("failed to append to WAL: %w", err)
		}
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	s.pool.Notify()
	return "", nil
}

// checkWritable rejects writes when the project is corrupted/dangerous or the
// WAL is full.
func (s *FSGitStorage) checkWritable(namespace, projectName string) error {
	switch s.State(namespace, projectName) {
	case StateDangerous:
		return ErrProjectDangerous
	case StateCorrupted:
		return ErrProjectCorrupted
	}
	if s.wal.PendingCount() >= s.maxWALEntries {
		return ErrWriteDisabled
	}
	return nil
}

// atomicWriteFile writes data to a temp file in the same directory and renames
// it into place, so a reader never sees a partially-written file.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
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
		if name == ".git" || name == ".drafts" || name == ".shoka" {
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

// GetHistory returns the commit history for a specific file (git-backed).
func (s *FSGitStorage) GetHistory(namespace, projectName, path string, limit int) ([]CommitInfo, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	if _, err := r.Head(); err != nil {
		// No commits yet (e.g. a write is still pending in the WAL). Not an error.
		return []CommitInfo{}, nil
	}
	logOptions := &git.LogOptions{Order: git.LogOrderCommitterTime}
	if path != "" {
		fullPath := filepath.Join(projectPath, path)
		rel, err := filepath.Rel(projectPath, fullPath)
		if filepath.IsAbs(path) || err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
			return nil, fmt.Errorf("invalid file path: %s", path)
		}
		rel = filepath.ToSlash(rel)
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
// This is the "past access" API; it is git-backed and isolated from the write
// path's lock manager.
func (s *FSGitStorage) ReadFileAtVersion(namespace, projectName, path, hash string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	_, rel, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return "", fmt.Errorf("failed to open git repository: %w", err)
	}
	commit, err := r.CommitObject(plumbing.NewHash(hash))
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

// GetCurrentVersion returns the current etag (sha256 of the file's content), or
// "" if the file does not exist. It validates the path (rejecting traversal),
// takes no lock, and never touches git.
func (s *FSGitStorage) GetCurrentVersion(namespace, projectName, path string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	fullPath, _, err := relWithin(projectPath, path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", nil // absent → no version (mirrors the previous "" semantics)
	}
	return sha256Hex(content), nil
}

// --- WAL / worker observability (for get_server_info §8.6 and metrics §11) ---

// WALPending returns the number of WAL entries awaiting a git commit.
func (s *FSGitStorage) WALPending() int { return s.wal.PendingCount() }

// WALPendingBytes returns the summed size of pending WAL entries.
func (s *FSGitStorage) WALPendingBytes() int64 { return s.wal.PendingBytes() }

// WALWriteDisabled reports whether the WAL has backed up past its threshold.
func (s *FSGitStorage) WALWriteDisabled() bool { return s.wal.PendingCount() >= s.maxWALEntries }

// WALMaxEntries returns the configured write-disabled threshold.
func (s *FSGitStorage) WALMaxEntries() int { return s.maxWALEntries }

// WALOldestEntryAge returns the age of the oldest pending WAL entry.
func (s *FSGitStorage) WALOldestEntryAge() time.Duration { return s.wal.OldestEntryAge() }

// WorkerStats returns the background commit pool's stats.
func (s *FSGitStorage) WorkerStats() walworker.Stats { return s.pool.Stats() }

// CommitStats returns the cumulative successful and failed background-commit
// counts, for the shoka_wal_commits_total metric.
func (s *FSGitStorage) CommitStats() (success, failure int64) {
	st := s.pool.Stats()
	return st.CommitsTotal, st.CommitsFailed
}

// ProjectStates returns each tracked project ("namespace/project") mapped to its
// state string, for the shoka_project_state metric.
func (s *FSGitStorage) ProjectStates() map[string]string {
	src := s.AllStates()
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = string(v)
	}
	return out
}

// LockStats returns the file-lock manager's active leases and forced-release count.
func (s *FSGitStorage) LockStats() (activeLeases int, forcedReleases int64) {
	return len(s.locks.ActiveLeases()), s.locks.ForcedReleaseCount()
}

// WaitForWAL blocks until the WAL has drained (no pending entries) or timeout
// elapses. It returns true if the WAL drained. Intended for tests and for
// drift detection's "after the WAL has caught up" precondition.
func (s *FSGitStorage) WaitForWAL(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if s.wal.PendingCount() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

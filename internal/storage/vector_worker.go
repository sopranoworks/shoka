package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/vectorindex"
)

const (
	vectorQueueSize = 256
)

// vectorWorkItem is a file queued for background vectorization.
type vectorWorkItem struct {
	namespace string
	project   string
	path      string
	content   []byte
}

// StartVectorWorker starts the background vectorization goroutine. It processes
// enqueued items (from the write path) and runs periodic reconciliation sweeps.
// interval <= 0 disables the periodic sweep (items are still processed from the
// queue). The goroutine stops when ctx is cancelled. Safe to call multiple times;
// only the first call starts the worker.
func (s *FSGitStorage) StartVectorWorker(ctx context.Context, interval time.Duration) {
	if !s.VectorConfigured() {
		return
	}
	if !s.vecWorkerStarted.CompareAndSwap(false, true) {
		return
	}
	go s.vectorWorkerLoop(ctx, interval)
}

func (s *FSGitStorage) vectorWorkerLoop(ctx context.Context, interval time.Duration) {
	// Initial reconcile: vectorize all pre-existing project files that have no
	// vector entry yet (mirrors StartIndexSweep's immediate reconcileAllIndexes).
	s.reconcileAllVectors(ctx)
	// Drain items that arrived during the initial reconcile.
	s.drainVectorQueue(ctx)

	var ticker *time.Ticker
	var tickC <-chan time.Time
	if interval > 0 {
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		tickC = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.vecQueue:
			s.processVectorItem(ctx, item)
		case <-tickC:
			s.reconcileAllVectors(ctx)
		}
	}
}

func (s *FSGitStorage) drainVectorQueue(ctx context.Context) {
	for {
		select {
		case item := <-s.vecQueue:
			s.processVectorItem(ctx, item)
		default:
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *FSGitStorage) processVectorItem(ctx context.Context, item vectorWorkItem) {
	if ctx.Err() != nil {
		return
	}
	_, err := s.vectorEmbed(ctx, item.namespace, item.project, item.path, item.content)
	if err != nil {
		if errors.Is(err, vectorindex.ErrModelMismatch) {
			s.removeVectorFile(item.namespace, item.project)
			s.vecRebuilds.Add(1)
		}
		s.log().Warn("vector embed failed",
			"namespace", item.namespace, "project", item.project,
			"path", item.path, "err", err)
	}
}

// reconcileAllVectors scans all projects and vectorizes any files that are missing
// from the vector index. This is the sweep equivalent of reconcileAllIndexes.
func (s *FSGitStorage) reconcileAllVectors(ctx context.Context) {
	if !s.VectorConfigured() {
		return
	}
	s.vecSweepRuns.Add(1)
	projects, _ := s.discoverProjects()
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		s.reconcileProjectVectors(ctx, p.namespace, p.name)
	}
}

func (s *FSGitStorage) reconcileProjectVectors(ctx context.Context, namespace, projectName string) {
	cfg := s.currentVecConfig()
	if cfg == nil {
		return
	}

	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return
	}

	// Check if existing store has matching model/dimensions
	st := s.vectorForRead(namespace, projectName)
	if st != nil {
		dims := s.vecResolvedDims
		if dims == 0 && cfg.Dimensions > 0 {
			dims = cfg.Dimensions
		}
		if dims > 0 {
			if err := st.CheckModel(cfg.Model, dims); err != nil {
				s.log().Info("vector index model mismatch, rebuilding",
					"namespace", namespace, "project", projectName)
				s.removeVectorFile(namespace, projectName)
				st = nil
				s.vecRebuilds.Add(1)
			}
		}
	}

	// Walk project files and find those missing from the vector index
	files := workingTreeFiles(projectPath)
	if len(files) == 0 {
		return
	}

	var existingKeys map[string]bool
	if st != nil {
		keys, _ := st.Keys()
		existingKeys = make(map[string]bool, len(keys))
		for _, k := range keys {
			existingKeys[k] = true
		}
	}

	for rel, content := range files {
		if ctx.Err() != nil {
			return
		}
		if existingKeys != nil && existingKeys[rel] {
			continue
		}
		_, err := s.vectorEmbed(ctx, namespace, projectName, rel, content)
		if err != nil {
			if errors.Is(err, vectorindex.ErrModelMismatch) {
				s.removeVectorFile(namespace, projectName)
				s.vecRebuilds.Add(1)
				return
			}
			s.log().Warn("vector sweep embed failed",
				"namespace", namespace, "project", projectName,
				"path", rel, "err", err)
		}
	}

	// Remove entries for files that no longer exist
	if st != nil {
		keys, _ := st.Keys()
		for _, k := range keys {
			if _, exists := files[k]; !exists {
				_ = st.Delete(k)
			}
		}
	}
}

// workingTreeFiles walks a project's working tree and returns a map of
// project-relative paths to file content. Uses the same derivativeWalkSkip*
// predicates as the fulltext index so the corpus is consistent.
func workingTreeFiles(projectPath string) map[string][]byte {
	files := make(map[string][]byte)
	_ = filepath.WalkDir(projectPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != projectPath && derivativeWalkSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if derivativeWalkSkipFile(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(projectPath, p)
		if relErr != nil {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return files
}

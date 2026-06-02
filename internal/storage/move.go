package storage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/storage/wal"
)

// Move is the redesign's move entry point (used by the §8 tool layer and /ws/ui).
// It renames sourcePath to targetPath within one project, history-preserving, and
// — in the SAME atomic git commit — rewrites every inbound internal markdown link
// so the project stays internally consistent (the move-file directive). It
// returns the destination's new etag and the number of links rewritten.
//
// ifMatch carries a dual optimistic-concurrency semantic (§1.3): when the target
// already exists it validates the TARGET's etag (explicit-overwrite intent); when
// the target does not exist it validates the SOURCE's etag. A target that exists
// with no ifMatch is refused (a move never silently overwrites). All conflicts
// surface as *VersionConflictError carrying the relevant current etag, so the
// existing CONFLICT plumbing (MCP + /ws/ui) handles them unchanged.
func (s *FSGitStorage) Move(ctx context.Context, sessionID, namespace, projectName, sourcePath, targetPath string, ifMatch *string) (string, int, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", 0, err
	}
	srcFull, srcRel, err := relWithin(projectPath, sourcePath)
	if err != nil {
		return "", 0, err
	}
	dstFull, dstRel, err := relWithin(projectPath, targetPath)
	if err != nil {
		return "", 0, err
	}
	if srcRel == dstRel {
		return "", 0, fmt.Errorf("move: source and target are the same path: %s", srcRel)
	}
	if err := s.checkWritable(namespace, projectName); err != nil {
		return "", 0, err
	}

	// Phase 1 (no lock): discover the set of referrer files whose links must be
	// rewritten, so we know the full lock set before acquiring it. The authoritative
	// recompute happens again under the locks below; this pass only finds candidates.
	candidates, err := s.discoverReferrers(projectPath, srcRel, dstRel)
	if err != nil {
		return "", 0, err
	}

	// Lock set: source, destination, and every candidate referrer. WithLocks takes
	// them as one atomic acquisition in canonical stripe order (deadlock-free).
	lockPaths := make([]string, 0, len(candidates)+2)
	lockPaths = append(lockPaths, srcFull, dstFull)
	for _, c := range candidates {
		lockPaths = append(lockPaths, filepath.Join(projectPath, filepath.FromSlash(c)))
	}

	var newEtag string
	var aux []wal.AuxFile
	var movedContent []byte

	lockErr := s.locks.WithLocks(ctx, sessionID, lockPaths, func() error {
		// Source must still exist.
		src, rerr := os.ReadFile(srcFull)
		if rerr != nil {
			return fmt.Errorf("move: source not found: %w", rerr)
		}
		srcEtag := sha256Hex(src)

		// Dual if_match semantic + no-silent-overwrite policy (§1.3).
		dstExisting, dstErr := os.ReadFile(dstFull)
		dstExists := dstErr == nil
		if dstExists {
			dstEtag := sha256Hex(dstExisting)
			if ifMatch == nil {
				// Target exists and the caller did not opt into overwrite: refuse,
				// handing back the target's etag so the caller can decide.
				return &VersionConflictError{Expected: "", Current: dstEtag}
			}
			if *ifMatch != dstEtag {
				return &VersionConflictError{Expected: *ifMatch, Current: dstEtag}
			}
		} else if ifMatch != nil && *ifMatch != srcEtag {
			// Target absent: ifMatch guards the source against a mid-air change.
			return &VersionConflictError{Expected: *ifMatch, Current: srcEtag}
		}

		movedContent = src
		newEtag = srcEtag // bytes are unchanged, so the destination etag == source etag

		// Authoritative link rewrite under the locks: re-read each candidate and
		// recompute, so the committed Aux reflects locked content.
		aux = aux[:0]
		for _, c := range candidates {
			cFull := filepath.Join(projectPath, filepath.FromSlash(c))
			data, derr := os.ReadFile(cFull)
			if derr != nil {
				continue // vanished since discovery; skip
			}
			rewritten, n := rewriteLinks(data, c, srcRel, dstRel)
			if n == 0 {
				continue
			}
			aux = append(aux, wal.AuxFile{Path: c, Content: rewritten})
		}

		// Working tree mutations (layer 1, ground truth). Order: write destination,
		// rewrite referrers, then remove the source last — so a crash mid-operation
		// never leaves the content unreachable from both paths.
		if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
			return fmt.Errorf("move: create target dir: %w", err)
		}
		if err := atomicWriteFile(dstFull, movedContent); err != nil {
			return fmt.Errorf("move: write target: %w", err)
		}
		for _, a := range aux {
			aFull := filepath.Join(projectPath, filepath.FromSlash(a.Path))
			if err := atomicWriteFile(aFull, a.Content); err != nil {
				return fmt.Errorf("move: rewrite %s: %w", a.Path, err)
			}
		}
		if err := os.Remove(srcFull); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("move: remove source: %w", err)
		}

		// Catalog (best-effort, mirrors write/delete): disown source, adopt dest and
		// the rewritten referrers.
		s.catalogDelete(namespace, projectName, srcRel)
		s.catalogPut(namespace, projectName, dstRel, newEtag, len(movedContent), dstFull)
		for _, a := range aux {
			aFull := filepath.Join(projectPath, filepath.FromSlash(a.Path))
			s.catalogPut(namespace, projectName, a.Path, sha256Hex(a.Content), len(a.Content), aFull)
		}

		// One WAL entry → one atomic git commit (rename + every rewrite).
		id := identity.Resolve(ctx, s.identityDefaults)
		if _, err := s.wal.Append(wal.Entry{
			Namespace:    namespace,
			Project:      projectName,
			Path:         dstRel,
			MoveFrom:     srcRel,
			Op:           "move",
			Content:      movedContent,
			Aux:          aux,
			UserName:     id.UserName,
			UserEmail:    id.UserEmail,
			AgentName:    id.AgentName,
			WorkerID:     id.WorkerID,
			AuthorIsUser: id.AuthorIsUser,
		}); err != nil {
			return fmt.Errorf("move: append to WAL: %w", err)
		}
		return nil
	})
	if lockErr != nil {
		return "", 0, lockErr
	}

	s.pool.Notify()
	// file.move carries both source and target (§1.5); the ctx-borne sender excludes
	// the originating connection from its own event (2026-06-01 sender-exclusion).
	s.notify.NotifyMoveFrom(notify.SenderFrom(ctx), namespace+"/"+projectName, srcRel, dstRel)
	return newEtag, len(aux), nil
}

// discoverReferrers walks the project's markdown files (excluding the source and
// destination themselves) and returns the slash-relative paths whose inbound
// links to srcRel would change — the candidate set MoveFile then locks and
// re-checks authoritatively. .git and .drafts are skipped.
func (s *FSGitStorage) discoverReferrers(projectPath, srcRel, dstRel string) ([]string, error) {
	if _, err := os.Stat(projectPath); err != nil {
		// No project directory: there are no referrers to find. The locked phase
		// then fails cleanly on "source not found" rather than a raw walk error.
		return nil, nil
	}
	var referrers []string
	walkErr := filepath.WalkDir(projectPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != projectPath && (d.Name() == ".git" || d.Name() == ".drafts") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(projectPath, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == srcRel || rel == dstRel {
			return nil // never rewrite the moved file or the overwrite target
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		if _, n := rewriteLinks(data, rel, srcRel, dstRel); n > 0 {
			referrers = append(referrers, rel)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("move: scan referrers: %w", walkErr)
	}
	sort.Strings(referrers)
	return referrers, nil
}

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
// It renames sourcePath to targetPath within one project as a PURE, atomic,
// history-preserving git rename — a path change and nothing else. Inbound-link
// auto-update was decoupled and disabled on 2026-06-03 (backlog B-33); the second
// return value (links rewritten) is therefore always 0. The dormant rewriter is
// retained for re-enablement once a reverse-link index exists — see
// rewriteInboundLinksForMove.
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

	// Link auto-update on move is DECOUPLED and disabled (backlog B-33,
	// directives/2026-06-03-shoka-move-file-disable-link-rewrite.md). A move is now
	// a PURE path change: an atomic, history-preserving git rename and nothing else.
	// The move performs NO referrer scan, the WAL entry carries NO Aux, and
	// linksRewritten is ALWAYS 0. The inbound-link rewriter is retained dormant
	// (rewriteInboundLinksForMove + discoverReferrers + linkrewrite.go) so a future
	// reverse-link-index directive re-enables it by RE-WIRING, not re-implementing —
	// see rewriteInboundLinksForMove for the re-enablement seam. The per-move
	// full-corpus referrer scan the original implementation did is exactly what the
	// operator judged not viable; it is gone, not parked behind a flag.

	// Lock set: source + target only. WithLocks (not WithLock) is still required —
	// the two paths usually hash to different stripes, and the stripe mutexes are
	// non-reentrant + hash-shared, so a deadlock-free 2-path atomic acquisition
	// needs WithLocks' canonical-order, de-duplicated locking. The primitive is kept
	// (B-33 §"Lock scope"): it now locks ≤2 paths instead of source+target+referrers.
	lockPaths := []string{srcFull, dstFull}

	var newEtag string
	var movedContent []byte

	lockErr := s.locks.WithLocks(ctx, sessionID, lockPaths, func() error {
		// Source must still exist.
		src, rerr := os.ReadFile(srcFull)
		if rerr != nil {
			return fmt.Errorf("move: source not found: %w", rerr)
		}
		srcEtag := sha256Hex(src)

		// Dual if_match semantic + no-silent-overwrite policy (§1.3). Unchanged by
		// the link-rewrite decoupling — it lives entirely here, independent of any
		// referrer handling.
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

		// Working tree mutations (layer 1, ground truth). Order: write destination,
		// then remove the source last — so a crash mid-operation never leaves the
		// content unreachable from both paths. No referrer files are touched (a move
		// is a pure rename).
		if err := os.MkdirAll(filepath.Dir(dstFull), 0o755); err != nil {
			return fmt.Errorf("move: create target dir: %w", err)
		}
		if err := atomicWriteFile(dstFull, movedContent); err != nil {
			return fmt.Errorf("move: write target: %w", err)
		}
		if err := os.Remove(srcFull); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("move: remove source: %w", err)
		}

		// Catalog (best-effort, mirrors write/delete): disown source, adopt dest.
		s.catalogDelete(namespace, projectName, srcRel)
		s.catalogPut(namespace, projectName, dstRel, newEtag, len(movedContent), dstFull)

		// One WAL entry → one atomic git commit (the rename only; no Aux).
		id := identity.Resolve(ctx, s.identityDefaults)
		if _, err := s.wal.Append(wal.Entry{
			Namespace:    namespace,
			Project:      projectName,
			Path:         dstRel,
			MoveFrom:     srcRel,
			Op:           "move",
			Content:      movedContent,
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
	// linksRewritten is always 0: link auto-update on move is disabled (B-33).
	return newEtag, 0, nil
}

// rewriteInboundLinksForMove is the DORMANT inbound-link-rewrite pipeline for a
// move, and the B-33 RE-ENABLEMENT SEAM. storage.Move no longer calls it: link
// auto-update on move was decoupled and disabled on 2026-06-03
// (directives/2026-06-03-shoka-move-file-disable-link-rewrite.md) because finding
// referrers required rescanning the whole project's markdown on EVERY move (there
// is no reverse-link index), which the operator judged not viable. The discovery
// (discoverReferrers), the goldmark rewriter (rewriteLinks, linkrewrite.go), and
// the Aux assembly here are all retained intact and tested directly
// (TestMove_DormantInboundLinkRewriteSeam, TestRewriteLinks) so re-enabling
// link-update-on-move is a RE-WIRE, not a re-implementation: once a reverse-link
// index exists, call this from Move under the locks (lock src + dst + each returned
// referrer path), feed the returned []wal.AuxFile into the WAL move entry's Aux,
// restore the Aux fold in buildMoveTree, and return len(aux) as linksRewritten.
func (s *FSGitStorage) rewriteInboundLinksForMove(projectPath, srcRel, dstRel string) ([]wal.AuxFile, error) {
	candidates, err := s.discoverReferrers(projectPath, srcRel, dstRel)
	if err != nil {
		return nil, err
	}
	var aux []wal.AuxFile
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
	return aux, nil
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

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// fix_links — the asynchronous, worker-driven link repair after a move (I3, the
// 2026-06-04 directive). It is the ONLY write-to-truth edge in the indexing line,
// so it is built so a broken index can only make it do LESS (truth-scan / skip),
// never something WRONG (miss or over-rewrite referrers).
//
// move_file stays a pure synchronous rename; fix_links runs later, off the
// request path, kicked once per move with (namespace, project, src, dst). For
// each file that referenced src it rewrites the link to point at dst as an
// ORDINARY if_match write through writeTransformed — read the referrer at its
// current etag E, rewrite via the idempotent rewriteLinks, write if_match=E. On
// an if_match conflict (the referrer changed meanwhile — an operator edit, or
// another move) it backs off and leaves the referrer for a later kick, never
// clobbering a concurrent edit.

// fixLinksKickBuffer bounds the in-memory kick queue. A burst beyond this drops
// surplus kicks (logged); since fix_links has no periodic backstop, a dropped
// kick leaves a stale-but-recoverable link the three tenets absorb. Sized
// generously so only a pathological move storm overflows it.
const fixLinksKickBuffer = 256

// fixLinksKick is one post-move reconciliation request. It carries everything the
// worker needs; "R changed" is never a kick — only a move enqueues one, keyed to a
// single (src→dst), which is what makes the reconciliation converge (a referrer
// rewrite goes through writeTransformed but enqueues no further kick).
type fixLinksKick struct {
	namespace string
	project   string
	src       string
	dst       string
}

// enqueueFixLinks does a non-blocking send of a post-move kick. It never blocks
// the caller (Move stays a pure rename); on a full channel the kick is dropped
// and logged. nil-channel-safe for storage built without the worker.
func (s *FSGitStorage) enqueueFixLinks(namespace, projectName, src, dst string) {
	if s.fixLinksKicks == nil {
		return
	}
	select {
	case s.fixLinksKicks <- fixLinksKick{namespace: namespace, project: projectName, src: src, dst: dst}:
	default:
		s.log().Warn("fix_links: kick channel full, dropping post-move kick (a later move or manual reindex recovers)",
			"project", projectKey(namespace, projectName), "src", src, "dst", dst)
	}
}

// fixLinks reconciles the inbound links of a single move src→dst. It is safe to
// run repeatedly: rewriteLinks is idempotent, so a referrer already pointing at
// dst is a no-op. It never returns an error — repair is best-effort, like the
// rest of the derivative layer; a transient failure is left for a later kick.
func (s *FSGitStorage) fixLinks(ctx context.Context, namespace, projectName, src, dst string) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return
	}
	referrers, err := s.findReferrersForFix(namespace, projectName, projectPath, src, dst)
	if err != nil {
		s.log().Warn("fix_links: find referrers failed",
			"project", projectKey(namespace, projectName), "src", src, "dst", dst, "err", err)
		return
	}
	for _, ref := range referrers {
		// Never rewrite the moved file itself: src no longer exists, and dst's own
		// outbound links are deliberately left untouched (preserve git log --follow).
		// discoverReferrers already excludes both; the index path is filtered here.
		if ref == src || ref == dst {
			continue
		}
		s.fixLinksRewriteReferrer(ctx, namespace, projectName, projectPath, ref, src, dst)
	}
}

// findReferrersForFix returns the files that reference src. When the index is
// healthy it is the reverse-link lookup (one bucket scan); otherwise it is the
// truth-scan over the working tree (discoverReferrers). A broken/absent/stale
// index therefore makes fix_links truth-scan — never rewrite from partial
// referrer knowledge — so a healthy lookup and a truth-scan repair identically
// (both are .md-only by construction).
func (s *FSGitStorage) findReferrersForFix(namespace, projectName, projectPath, src, dst string) ([]string, error) {
	if s.IndexHealthy(namespace, projectName) {
		if ix := s.indexForRead(namespace, projectName); ix != nil {
			return ix.Referrers(src)
		}
	}
	return s.discoverReferrers(projectPath, src, dst)
}

// fixLinksRewriteReferrer repairs one referrer's link to src, repointing it at
// dst, as an if_match write. It reads the referrer's current bytes (etag E) off
// the request path, skips when rewriteLinks finds nothing to change (idempotent
// no-op), and otherwise writes the rewrite back through writeTransformed with
// if_match=E. A VersionConflictError means the referrer changed between the read
// and the write — fix_links backs off and leaves it for a later kick rather than
// clobber the concurrent change.
func (s *FSGitStorage) fixLinksRewriteReferrer(ctx context.Context, namespace, projectName, projectPath, ref, src, dst string) {
	full := filepath.Join(projectPath, filepath.FromSlash(ref))
	data, rerr := os.ReadFile(full)
	if rerr != nil {
		return // vanished since discovery; skip
	}
	if _, n := rewriteLinks(data, ref, src, dst); n == 0 {
		return // already correct / no longer references src — nothing to do
	}
	expected := sha256Hex(data)
	_, werr := s.writeTransformed(ctx, "", namespace, projectName, ref, &expected,
		func(current []byte) ([]byte, error) {
			// if_match=expected guarantees current == data, so this re-rewrite under
			// the lock equals the one checked above; recomputing keeps the rewrite a
			// pure function of the locked bytes.
			out, _ := rewriteLinks(current, ref, src, dst)
			return out, nil
		})
	if werr == nil {
		return
	}
	var conflict *VersionConflictError
	if errors.As(werr, &conflict) {
		// Concurrent edit: do not retry tightly, do not force. A later kick (or a
		// future move) reconciles against the then-current bytes. Never clobbers.
		return
	}
	s.log().Warn("fix_links: rewrite referrer failed",
		"project", projectKey(namespace, projectName), "referrer", ref, "src", src, "dst", dst, "err", werr)
}

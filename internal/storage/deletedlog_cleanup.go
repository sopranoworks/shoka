package storage

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/deletedlog"
)

// One-time cleanup of OLD JUNK deleted-logs (2026-06-18). After the lazy-create fix a
// <p>.deleted.db is created only on a real deletion, and after the origin-marker fix every
// such log carries a marker. The files left over from the retired over-broad lazy-create
// (created on a plain op:"write", e.g. an existing maintenance.deleted.db) have NO marker and
// hold zero deletion records — that, and only that, is what this removes. A MARKED log is
// never removed (even if revives emptied it — a legitimate live log); a log with ≥1 record is
// never removed (no data loss). Both gates are mandatory and O(1) per file (a marker key
// lookup + a first-key emptiness probe) — never a git walk, never a full record scan.
//
// It runs as a ONE-TIME startup pass guarded by a done-flag, NOT a per-boot full scan, and
// NOT a remotely-invoked op: per the safe-removal report the startup pass is the safest
// shape — it runs before the listeners open, so the server owns every handle and there is no
// live-handle race, and removeDeletedLogFile closes the cached handle before unlinking. The
// done-flag stops it re-running every boot; a crash before the flag is written just re-runs
// it next boot (idempotent — it only ever removes junk).

const deletedLogCleanupFlag = "deleted-log-cleanup-v1.done"

// CleanupUnmarkedEmptyDeletedLogs removes every <p>.deleted.db that has NO origin marker AND
// holds zero deletion records, and returns the project keys whose junk log was removed. It
// enumerates only the .deleted.db files that exist on disk (bounded, small); each check is
// O(1). A file it cannot open/read is left untouched (conservative — never remove what cannot
// be verified empty).
func (s *FSGitStorage) CleanupUnmarkedEmptyDeletedLogs() ([]string, error) {
	if !s.deletedLogEnabled {
		return nil, nil
	}
	nsEntries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, ns := range nsEntries {
		if !ns.IsDir() || strings.HasPrefix(ns.Name(), ".") {
			continue
		}
		nsDir := filepath.Join(s.baseDir, ns.Name())
		files, ferr := os.ReadDir(nsDir)
		if ferr != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".deleted.db") {
				continue
			}
			// Handle both legacy (<p>.deleted.db) and current (@<p>.deleted.db) naming.
			proj := strings.TrimSuffix(f.Name(), ".deleted.db")
			if strings.HasPrefix(proj, "@") {
				proj = proj[1:]
			}
			// Open at the actual on-disk path (not via deletedLogPath, which always
			// returns the @-prefixed path and would miss legacy files).
			actualPath := filepath.Join(nsDir, f.Name())
			st, oerr := deletedlog.Open(actualPath)
			if oerr != nil {
				continue // unreadable: cannot verify it is empty — leave it
			}
			if marked, merr := st.HasOriginMarker(); merr != nil || marked {
				_ = st.Close()
				continue // GATE 1: a marked log (legitimate) is NEVER removed
			}
			if empty, eerr := st.IsEmpty(); eerr != nil || !empty {
				_ = st.Close()
				continue // GATE 2: a log with ≥1 record is NEVER removed (no data loss)
			}
			_ = st.Close()
			// Old junk: unmarked AND empty. Close any cached handle, then unlink the
			// actual file (which may be at the legacy or current path).
			s.evictDeletedLogHandle(ns.Name(), proj)
			_ = os.Remove(actualPath)
			removed = append(removed, projectKey(ns.Name(), proj))
		}
	}
	return removed, nil
}

// deletedLogCleanupOnce runs CleanupUnmarkedEmptyDeletedLogs exactly once per data dir,
// guarded by a done-flag under <base>/.shoka, so it does not re-scan every boot. A scan error
// (or a crash before the flag is written) leaves the flag absent so it retries next boot —
// harmless, since it only ever removes junk.
func (s *FSGitStorage) deletedLogCleanupOnce() {
	if !s.deletedLogEnabled {
		return
	}
	flag := filepath.Join(s.baseDir, ".shoka", deletedLogCleanupFlag)
	if _, err := os.Stat(flag); err == nil {
		return // already done — do NOT re-scan
	}
	removed, err := s.CleanupUnmarkedEmptyDeletedLogs()
	if err != nil {
		s.log().Warn("deleted-log cleanup: scan failed; will retry next boot", "err", err)
		return // leave the flag absent → retry next boot
	}
	if len(removed) > 0 {
		s.log().Info("deleted-log cleanup: removed old unmarked empty deleted-logs",
			"count", len(removed), "projects", removed)
	}
	if mkErr := os.MkdirAll(filepath.Dir(flag), 0o755); mkErr == nil {
		if werr := os.WriteFile(flag, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); werr != nil {
			s.log().Warn("deleted-log cleanup: write done-flag failed; may re-run next boot", "err", werr)
		}
	}
}

package storage

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefRewriteRace_GetHistoryAndListFilesSince is the regression pin for the
// 2026-06-02 "WAL-vs-read" race investigation (directive
// 2026-06-01-shoka-wal-vs-read-race-investigation.md).
//
// MECHANISM (see reports/progress/2026-06-02-wal-vs-read-race-coldread-and-reframe.md
// for the full source-cited write-up). The background commit worker advances an
// existing branch ref via go-git's setRefRwfs, which opens refs/heads/<branch>
// with O_TRUNC *before* taking the flock and *before* writing the new hash — so
// the loose ref file is 0 bytes for a window on every commit after the first.
// Shoka's git-backed reads (GetHistory, ListFilesSince) resolve HEAD lock-free on
// an independent *git.Repository handle; if they read the ref during that window
// they see an empty file -> ErrEmptyRefFile -> packedRef (none) ->
// plumbing.ErrReferenceNotFound. This is NOT a WAL-append-vs-commit timing gap
// (that "no HEAD yet" state is already handled correctly); it is a filesystem-level
// TOCTOU on the ref file, which is exactly why `-race` never flags it (it
// instruments shared memory, not file I/O across separate handles).
//
// This is a NATURAL-PATH reproduction (directive §3.3): it calls only the public
// APIs under intense-but-ordinary concurrent load. There is NO synthetic timing
// manipulation — no worker pauses, no injected sleeps, no forced channel/Cond
// orderings. Repetition is the only mechanism.
//
// FOUR manifestations are counted (the operator's Option 1 — both modes, both
// APIs):
//   - GetHistory      loud  : returns "reference not found"
//   - GetHistory      silent: returns an empty history though >=1 commit is durable
//   - ListFilesSince  loud  : returns "reference not found"
//   - ListFilesSince  silent: returns no changes though >=1 commit is durable
//
// VERIFICATION CONTRACT (binds the future fix directive): this test asserts ZERO
// manifestations. On the current (unfixed) tree it is RED — it demonstrates the
// race. After the fix it must be GREEN with the same (or larger) load. Because a
// committed always-on red test would break the `go test ./...` gate, the test is
// opt-in: set SHOKA_RACE_REPRO=1 to run it. The fix directive runs it with that
// env var set and requires zero manifestations; this investigation ran it the same
// way to measure the rate recorded in the progress note.
func TestRefRewriteRace_GetHistoryAndListFilesSince(t *testing.T) {
	if os.Getenv("SHOKA_RACE_REPRO") == "" {
		t.Skip("opt-in race reproduction: set SHOKA_RACE_REPRO=1 to run. " +
			"Asserts ZERO ref-rewrite-race manifestations — RED on the unfixed tree " +
			"(demonstrates the race), GREEN after the fix (its verification criterion). " +
			"See reports/progress/2026-06-02-wal-vs-read-race-coldread-and-reframe.md.")
	}

	s := newTestStorage(t) // creates ns/proj, starts the worker pool, Close+cleanup
	ctx := context.Background()
	const ns, proj = "ns", "proj"

	// Seed one durable commit. After this the branch ref EXISTS and history is
	// non-empty FOREVER (git commits only accumulate), so any later empty result
	// is an unambiguous race manifestation — never a legitimate "no commits yet".
	if _, err := s.Write(ctx, "", ns, proj, "seed.md", "seed", nil); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	drain(t, s)

	const (
		writers         = 3
		writesPerWriter = 600 // bounds history depth: ~1800 commits total
		ghReaders       = 4
		lfsReaders      = 3
		drainTimeout    = 60 * time.Second // safety cap on the worker draining the WAL
	)

	var (
		ghErr, ghEmpty    atomic.Int64
		lfsErr, lfsEmpty  atomic.Int64
		ghReads, lfsReads atomic.Int64
		unexpected        atomic.Int64
	)

	stop := make(chan struct{}) // closed once writers are done AND the WAL has drained
	var writerWG, readerWG sync.WaitGroup

	// Writers: a BOUNDED stream of DISTINCT-content writes to the same project.
	// Distinct content matters — identical content yields newTree==baseTree and
	// commitEntry returns early without advancing the ref (commit.go:62), so there
	// would be no ref rewrite and no race. Each distinct write => one real commit
	// => one O_TRUNC ref rewrite by the single per-project worker. No sleeps.
	// Bounding the write count bounds history depth (so ListFilesSince's full walk
	// stays cheap); the ref keeps churning AFTER the writers finish, while the
	// worker drains the queued WAL entries — and the readers run throughout.
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func(w int) {
			defer writerWG.Done()
			path := fmt.Sprintf("hot-%d.md", w)
			for i := 0; i < writesPerWriter; i++ {
				_, _ = s.Write(ctx, "", ns, proj, path, fmt.Sprintf("v%d", i), nil)
			}
		}(w)
	}

	// Controller: once every write is queued and the worker has committed them all
	// (the ref has stopped churning), tell the readers to stop. The drain wait is
	// capped so a stall can never hang the test.
	go func() {
		writerWG.Wait()
		s.WaitForWAL(drainTimeout)
		close(stop)
	}()

	// GetHistory readers. limit=1 keeps each call cheap: the race is in HEAD
	// resolution inside r.Log (which runs before any commit is walked), so limit=1
	// still exposes both the loud error and the silent empty. Loop until the ref
	// stops churning (stop closed).
	for r := 0; r < ghReaders; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				hist, err := s.GetHistory(ns, proj, "", 1)
				ghReads.Add(1)
				if err != nil {
					if strings.Contains(err.Error(), "reference not found") {
						ghErr.Add(1)
					} else {
						unexpected.Add(1)
						t.Errorf("GetHistory unexpected error: %v", err)
					}
					continue
				}
				if len(hist) == 0 {
					ghEmpty.Add(1) // spurious: a seed commit is durably committed
				}
			}
		}()
	}

	// ListFilesSince readers. An ancient 'since' means every commit qualifies, so a
	// correct result is always non-empty once history exists; an empty result means
	// r.Head() (discovery.go:46) read the truncated ref and returned early (the
	// silent manifestation). The empty-return happens before the walk, so detecting
	// the manifestation stays cheap even though the non-racing path walks history.
	for r := 0; r < lfsReaders; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch, err := s.ListFilesSince(ns, proj, "1970-01-01T00:00:00Z")
				lfsReads.Add(1)
				if err != nil {
					if strings.Contains(err.Error(), "reference not found") {
						lfsErr.Add(1)
					} else {
						unexpected.Add(1)
						t.Errorf("ListFilesSince unexpected error: %v", err)
					}
					continue
				}
				if len(ch) == 0 {
					lfsEmpty.Add(1) // spurious: a seed commit is durably committed
				}
			}
		}()
	}

	readerWG.Wait()
	writerWG.Wait()

	total := ghErr.Load() + ghEmpty.Load() + lfsErr.Load() + lfsEmpty.Load()
	t.Logf("ref-rewrite race over %d GetHistory + %d ListFilesSince reads:\n"+
		"  GetHistory     loud   'reference not found' = %d\n"+
		"  GetHistory     silent spurious-empty        = %d\n"+
		"  ListFilesSince loud   'reference not found' = %d\n"+
		"  ListFilesSince silent spurious-empty        = %d\n"+
		"  TOTAL manifestations = %d (unexpected errors = %d)",
		ghReads.Load(), lfsReads.Load(),
		ghErr.Load(), ghEmpty.Load(), lfsErr.Load(), lfsEmpty.Load(),
		total, unexpected.Load())

	if total > 0 {
		t.Fatalf("ref-rewrite race reproduced: %d manifestations (want 0; "+
			"this is the fix directive's zero-failure verification target)", total)
	}
}

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRecoveryUnderConcurrentReads validates that the migrated recovery paths
// (RepairTrackedChanges / RestoreToLatest) close the latent ref-write race that
// the 2026-06-02 ref-write-race fix did not cover — because recovery, unlike the
// commit worker, was never exercised concurrently with live reads.
//
// Before the Anchor-3 migration, RepairTrackedChanges advanced the ref via
// go-git's w.Commit -> setHEADCommit -> SetReference (O_TRUNC before flock), and
// RestoreToLatest via w.Reset(HardReset) -> the same path. If a future workflow
// ever ran recovery while readers were live, those non-atomic ref writes would
// surface the same "reference not found" / spurious-empty manifestations the
// commit-worker race did. The migration routes RepairTrackedChanges through the
// atomic advanceHead funnel and removes RestoreToLatest's ref write entirely
// (HEAD is restored to itself), so the race cannot occur.
//
// This is the same NATURAL-PATH shape as TestRefRewriteRace: recovery is the sole
// writer (recovery runs on a halted project — the walworker is idle here), readers
// loop on the ref-resolving APIs, and ZERO manifestations are asserted. No
// synthetic timing. Opt-in via SHOKA_RACE_REPRO=1 (same gate as the sibling test)
// so a committed always-on red test cannot break the `go test ./...` gate.
func TestRecoveryUnderConcurrentReads(t *testing.T) {
	if os.Getenv("SHOKA_RACE_REPRO") == "" {
		t.Skip("opt-in race reproduction: set SHOKA_RACE_REPRO=1 to run. " +
			"Asserts ZERO ref-rewrite-race manifestations from recovery under concurrent " +
			"live reads — GREEN on the Anchor-3-migrated tree, RED if recovery regresses " +
			"to go-git's w.Commit / w.Reset ref writes.")
	}

	s := newTestStorage(t)
	ctx := context.Background()
	const ns, proj = "ns", "proj"

	// Seed a durable commit so history is non-empty forever; any later empty read
	// is an unambiguous manifestation, never a legitimate "no commits yet".
	if _, err := s.Write(ctx, "", ns, proj, "seed.md", "seed", nil); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	drain(t, s)

	projectPath, err := s.getProjectPath(ns, proj)
	if err != nil {
		t.Fatalf("project path: %v", err)
	}
	seedFile := filepath.Join(projectPath, "seed.md")

	const (
		recoveryCycles = 400
		ghReaders      = 4
		lfsReaders     = 3
	)

	var (
		ghErr, ghEmpty    atomic.Int64
		lfsErr, lfsEmpty  atomic.Int64
		ghReads, lfsReads atomic.Int64
		repairs, restores atomic.Int64
		unexpected        atomic.Int64
	)

	stop := make(chan struct{})
	var driverWG, readerWG sync.WaitGroup

	// Driver: the sole writer. Each cycle creates a TRACKED modification on disk
	// (distinct content -> a real adopted commit -> one atomic ref advance), then
	// runs RepairTrackedChanges. Every 20th cycle also exercises RestoreToLatest
	// (which writes no ref) under the same live readers. No sleeps — repetition is
	// the only mechanism.
	driverWG.Add(1)
	go func() {
		defer driverWG.Done()
		defer close(stop)
		for i := 0; i < recoveryCycles; i++ {
			if werr := os.WriteFile(seedFile, []byte(fmt.Sprintf("rev-%d", i)), 0o644); werr != nil {
				unexpected.Add(1)
				t.Errorf("write tracked change: %v", werr)
				return
			}
			if _, rerr := s.RepairTrackedChanges(ctx, ns, proj); rerr != nil {
				unexpected.Add(1)
				t.Errorf("RepairTrackedChanges: %v", rerr)
				return
			}
			repairs.Add(1)

			if i%20 == 0 {
				// Transient modification, then discard it via RestoreToLatest.
				if werr := os.WriteFile(seedFile, []byte("transient-junk"), 0o644); werr != nil {
					unexpected.Add(1)
					t.Errorf("write transient change: %v", werr)
					return
				}
				if rerr := s.RestoreToLatest(ctx, ns, proj); rerr != nil {
					unexpected.Add(1)
					t.Errorf("RestoreToLatest: %v", rerr)
					return
				}
				restores.Add(1)
			}
		}
	}()

	// GetHistory readers (limit=1 keeps each call cheap; the race is in HEAD
	// resolution before any commit is walked).
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
					ghEmpty.Add(1) // spurious: the seed commit is durable
				}
			}
		}()
	}

	// ListFilesSince readers (ancient 'since' => every commit qualifies; empty is
	// a silent manifestation from a truncated-ref HEAD resolve).
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
					lfsEmpty.Add(1) // spurious: the seed commit is durable
				}
			}
		}()
	}

	driverWG.Wait()
	readerWG.Wait()

	total := ghErr.Load() + ghEmpty.Load() + lfsErr.Load() + lfsEmpty.Load()
	t.Logf("recovery-under-reads over %d RepairTrackedChanges + %d RestoreToLatest, "+
		"%d GetHistory + %d ListFilesSince reads:\n"+
		"  GetHistory     loud   'reference not found' = %d\n"+
		"  GetHistory     silent spurious-empty        = %d\n"+
		"  ListFilesSince loud   'reference not found' = %d\n"+
		"  ListFilesSince silent spurious-empty        = %d\n"+
		"  TOTAL manifestations = %d (unexpected errors = %d)",
		repairs.Load(), restores.Load(), ghReads.Load(), lfsReads.Load(),
		ghErr.Load(), ghEmpty.Load(), lfsErr.Load(), lfsEmpty.Load(),
		total, unexpected.Load())

	if total > 0 {
		t.Fatalf("recovery ref-write race reproduced: %d manifestations (want 0)", total)
	}
}

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// findProjectHealth returns the ProjectHealth for proj in nh, or a zero value.
func findProjectHealth(nh NamespaceHealth, proj string) ProjectHealth {
	for _, p := range nh.Projects {
		if p.Name == proj {
			return p
		}
	}
	return ProjectHealth{}
}

func foreignAdoptable(nh NamespaceHealth, name string) (found, adoptable bool) {
	for _, f := range nh.Foreign {
		if f.Name == name {
			return true, f.Adoptable
		}
	}
	return false, false
}

func hasOrphan(nh NamespaceHealth, name string) bool {
	for _, o := range nh.Orphaned {
		if o.Name == name {
			return true
		}
	}
	return false
}

// #1 (core) — a MISSING managed project is detected by the health check and is NOT
// auto-dropped; DropMissingProject removes the registry record only when called.
func TestHealth_MissingDetectedNotAutoDropped(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "gone"); err != nil {
		t.Fatal(err)
	}
	// Remove the project out-of-band (dir + sibling DBs) — the registry record remains.
	_ = os.RemoveAll(filepath.Join(s.baseDir, "ns", "gone"))
	_ = os.Remove(s.catalogPath("ns", "gone"))
	_ = os.Remove(s.indexPath("ns", "gone"))

	nh := s.CheckNamespaceHealth("ns")
	if ph := findProjectHealth(nh, "gone"); ph.State != projectStateMissing {
		t.Fatalf("project 'gone' state = %q, want %q", ph.State, projectStateMissing)
	}
	if nh.Healthy {
		t.Fatal("namespace with a missing project must be unhealthy")
	}
	// The check did NOT drop the record.
	if rec, _, _ := s.nsReg.Get("ns"); !contains(rec.Projects, "gone") {
		t.Fatal("health check must NOT auto-drop the missing project's registry record")
	}
	// DropMissingProject removes it only when explicitly called.
	if err := s.DropMissingProject("ns", "gone"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.nsReg.Get("ns"); contains(rec.Projects, "gone") {
		t.Fatal("DropMissingProject must remove the registry record")
	}
	// DropMissingProject refuses a PRESENT project (guard).
	if err := s.CreateProject("ns", "live"); err != nil {
		t.Fatal(err)
	}
	if err := s.DropMissingProject("ns", "live"); err == nil {
		t.Fatal("DropMissingProject must refuse a present project")
	}
}

// #2 — a CORRUPTED project (c9f6827 drift) is aggregated by namespace health, and
// RecoverCorruptedProject delegates to the existing ResyncToHead (non-destructive: it does
// not paper over genuine drift, and clears the project once it is clean again).
func TestHealth_CorruptedAggregatedAndRecovered(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "p", "a.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}
	filePath := filepath.Join(s.baseDir, "ns", "p", "a.md")
	// Hand-edit the tracked file out-of-band → genuine drift (catalog/HEAD vs tree).
	if err := os.WriteFile(filePath, []byte("v2-out-of-band"), 0o644); err != nil {
		t.Fatal(err)
	}

	nh := s.CheckNamespaceHealth("ns")
	if ph := findProjectHealth(nh, "p"); ph.State != string(StateCorrupted) {
		t.Fatalf("project p state = %q, want corrupted", ph.State)
	}
	if nh.Healthy {
		t.Fatal("namespace with a corrupted project must be unhealthy")
	}
	// RecoverCorruptedProject (→ ResyncToHead) is non-destructive: genuine drift stays.
	if st, _ := s.RecoverCorruptedProject("ns", "p"); st != StateCorrupted {
		t.Fatalf("RecoverCorruptedProject on genuine drift = %q, want corrupted (non-destructive)", st)
	}
	// Once the drift is resolved (file restored), recovery clears it to healthy.
	if err := os.WriteFile(filePath, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.RecoverCorruptedProject("ns", "p"); st != StateHealthy {
		t.Fatalf("RecoverCorruptedProject after restore = %q, want healthy", st)
	}
	if ph := findProjectHealth(s.CheckNamespaceHealth("ns"), "p"); ph.State != string(StateHealthy) {
		t.Fatalf("post-recovery health = %q, want healthy", ph.State)
	}
}

// #3 — an ORPHANED sibling (stray catalog/index .db, no project dir) is detected and
// CleanOrphanedSibling removes only the stray.
func TestHealth_OrphanedDetectedAndCleaned(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	if err := s.CreateProject("ns", "stray"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "stray", "a.md", "x", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)
	// Deregister + remove the project DIR only, leaving the sibling .db files stranded.
	_ = s.nsReg.RemoveProject("ns", "stray")
	s.evictProjectHandles("ns", "stray")
	_ = os.RemoveAll(filepath.Join(s.baseDir, "ns", "stray"))

	nh := s.CheckNamespaceHealth("ns")
	if !hasOrphan(nh, "stray") {
		t.Fatalf("stray .db must be reported orphaned: %+v", nh.Orphaned)
	}
	if nh.Healthy {
		t.Fatal("namespace with an orphaned sibling must be unhealthy")
	}
	if err := s.CleanOrphanedSibling("ns", "stray"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.catalogPath("ns", "stray")); !os.IsNotExist(err) {
		t.Error("orphaned catalog .db must be removed")
	}
	if _, err := os.Stat(s.indexPath("ns", "stray")); !os.IsNotExist(err) {
		t.Error("orphaned index .db must be removed")
	}
	if hasOrphan(s.CheckNamespaceHealth("ns"), "stray") {
		t.Error("orphan must be gone after cleanup")
	}
	// CleanOrphanedSibling refuses a LIVE project's catalog.
	if err := s.CreateProject("ns", "alive"); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanOrphanedSibling("ns", "alive"); err == nil {
		t.Fatal("CleanOrphanedSibling must refuse a live project")
	}
}

// #4 (core) — an UNTRACKED-FOREIGN dir does NOT make the namespace unhealthy; a valid .git
// untracked dir is flagged adoptable, an empty one is not; AdoptForeign registers a valid
// one only on explicit call.
func TestHealth_ForeignIgnorableButAdoptable(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "managed"); err != nil {
		t.Fatal(err)
	}
	// A valid .git project that is NOT managed (foreign-adoptable): create then deregister.
	if err := s.CreateProject("ns", "foreignproj"); err != nil {
		t.Fatal(err)
	}
	_ = s.nsReg.RemoveProject("ns", "foreignproj")
	// An empty, non-.git foreign dir (NOT adoptable).
	if err := os.MkdirAll(filepath.Join(s.baseDir, "ns", "emptyforeign"), 0o755); err != nil {
		t.Fatal(err)
	}

	nh := s.CheckNamespaceHealth("ns")
	if !nh.Healthy {
		t.Fatalf("foreign dirs must NOT make a namespace unhealthy: %+v", nh)
	}
	if found, adoptable := foreignAdoptable(nh, "foreignproj"); !found || !adoptable {
		t.Fatalf("a valid .git foreign dir must be flagged adoptable (found=%v adoptable=%v)", found, adoptable)
	}
	if found, adoptable := foreignAdoptable(nh, "emptyforeign"); !found || adoptable {
		t.Fatalf("an empty foreign dir must be listed non-adoptable (found=%v adoptable=%v)", found, adoptable)
	}
	// AdoptForeign registers the valid one ONLY on explicit call.
	if rec, _, _ := s.nsReg.Get("ns"); contains(rec.Projects, "foreignproj") {
		t.Fatal("health check must NOT auto-adopt a foreign project")
	}
	if err := s.AdoptForeign("ns", "foreignproj"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.nsReg.Get("ns"); !contains(rec.Projects, "foreignproj") {
		t.Fatal("AdoptForeign must register the foreign project")
	}
	// AdoptForeign refuses a non-.git (junk) dir.
	if err := s.AdoptForeign("ns", "emptyforeign"); err == nil {
		t.Fatal("AdoptForeign must refuse a non-.git dir")
	}
}

// #5 — the health check is read-only: it mutates neither the registry nor the on-disk
// layout (for a healthy managed set).
func TestHealth_ReadOnly(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "p", "a.md", "x", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	regBefore, _ := s.nsReg.List()
	recBefore, _, _ := s.nsReg.Get("ns")
	dirBefore := dirNames(t, filepath.Join(s.baseDir, "ns"))

	_ = s.CheckAllHealth()

	regAfter, _ := s.nsReg.List()
	recAfter, _, _ := s.nsReg.Get("ns")
	dirAfter := dirNames(t, filepath.Join(s.baseDir, "ns"))

	if !equalStrings(regBefore, regAfter) || !equalStrings(recBefore.Projects, recAfter.Projects) {
		t.Fatalf("health check mutated the registry: ns %v→%v, projects %v→%v",
			regBefore, regAfter, recBefore.Projects, recAfter.Projects)
	}
	if !equalStrings(dirBefore, dirAfter) {
		t.Fatalf("health check mutated the on-disk layout: %v→%v", dirBefore, dirAfter)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

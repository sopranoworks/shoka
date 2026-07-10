package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sopranoworks/shoka/internal/notify"
)

// makeLeftover creates a repo-less <ns>/<name>/ directory (a nested file inside, no
// .git) beside the storage's existing projects, optionally with a sibling
// <name>.db, and returns the matching leftover descriptor. It models the B-38.1
// remnant D4 relocates.
func makeLeftover(t *testing.T, s *FSGitStorage, ns, name string, withDB bool) leftover {
	t.Helper()
	treePath := filepath.Join(s.baseDir, ns, name)
	if err := os.MkdirAll(filepath.Join(treePath, "directives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(treePath, "directives", "x.md"), []byte("leftover body"), 0o644); err != nil {
		t.Fatal(err)
	}
	lf := leftover{namespace: ns, name: name, treePath: treePath}
	if withDB {
		dbPath := s.catalogPath(ns, name)
		if err := os.WriteFile(dbPath, []byte("fake catalog db"), 0o644); err != nil {
			t.Fatal(err)
		}
		lf.dbPaths = append(lf.dbPaths, dbPath)
	}
	return lf
}

// quarantineEventFor returns the lostfound.quarantined event for ns/name, or nil.
func quarantineEventFor(center *notify.Center, ns, name string) *notify.Event {
	events := center.Snapshot()
	for i := range events {
		if events[i].Kind == kindLostFoundQuarantined && events[i].Target == ns+"/"+name {
			return &events[i]
		}
	}
	return nil
}

// TestDiscoverProjects_SurfacesRepolessLeftover: discovery returns a repo-less dir on
// the leftover list (with its sibling .db) WITHOUT perturbing the primary project
// result (§3.1: surface, don't drop).
func TestDiscoverProjects_SurfacesRepolessLeftover(t *testing.T) {
	s := newStorageWithCenter(t, notify.NewCenter(16)) // creates real git project ns/proj
	lf := makeLeftover(t, s, "ns", "stray", true)

	projects, leftovers := s.discoverProjects()

	// Primary result unchanged: exactly the real project, leftover NOT among it.
	if len(projects) != 1 || projects[0].namespace != "ns" || projects[0].name != "proj" {
		t.Fatalf("primary projects = %+v, want exactly {ns, proj}", projects)
	}
	if len(leftovers) != 1 {
		t.Fatalf("leftovers = %+v, want exactly one", leftovers)
	}
	got := leftovers[0]
	if got.namespace != "ns" || got.name != "stray" {
		t.Fatalf("leftover id = %s/%s, want ns/stray", got.namespace, got.name)
	}
	if got.treePath != lf.treePath {
		t.Fatalf("leftover treePath = %q, want %q", got.treePath, lf.treePath)
	}
	if len(got.dbPaths) != 1 || got.dbPaths[0] != s.catalogPath("ns", "stray") {
		t.Fatalf("leftover dbPaths = %v, want [%q] (sibling .db present)", got.dbPaths, s.catalogPath("ns", "stray"))
	}
}

// TestDiscoverProjects_LeftoverWithoutDB: a leftover dir with no sibling .db is
// surfaced with an empty dbPath (the missing-.db case begins at discovery).
func TestDiscoverProjects_LeftoverWithoutDB(t *testing.T) {
	s := newStorageWithCenter(t, notify.NewCenter(16))
	makeLeftover(t, s, "ns", "stray", false)

	_, leftovers := s.discoverProjects()
	if len(leftovers) != 1 {
		t.Fatalf("leftovers = %+v, want exactly one", leftovers)
	}
	if len(leftovers[0].dbPaths) != 0 {
		t.Fatalf("dbPaths = %v, want empty (no sibling .db)", leftovers[0].dbPaths)
	}
}

// TestRelocateLeftovers_MovesTreeAndDBToLostFound: a repo-less leftover dir + its .db
// are relocated into one lost+found <ts> dir via depositTree, the sources are gone,
// content is intact, and a lostfound.quarantined NOTIFY is emitted (§3.2).
func TestRelocateLeftovers_MovesTreeAndDBToLostFound(t *testing.T) {
	center := notify.NewCenter(16)
	s := newStorageWithCenter(t, center)
	lf := makeLeftover(t, s, "ns", "stray", true)

	s.relocateLeftovers([]leftover{lf}, fixedTS)

	// Sources gone.
	if _, err := os.Stat(lf.treePath); !os.IsNotExist(err) {
		t.Fatalf("leftover tree still present, stat err=%v", err)
	}
	if _, err := os.Stat(lf.dbPaths[0]); !os.IsNotExist(err) {
		t.Fatalf("leftover .db still present, stat err=%v", err)
	}

	// Tree + .db grouped under one <ts> dir in the project's lost+found area.
	tsDir := filepath.Join(s.lostFoundRoot("ns", "stray"), fixedTS.UTC().Format(lostFoundTimeFormat))
	body, err := os.ReadFile(filepath.Join(tsDir, "stray", "directives", "x.md"))
	if err != nil {
		t.Fatalf("relocated tree content unreadable: %v", err)
	}
	if string(body) != "leftover body" {
		t.Fatalf("relocated content = %q, want %q", body, "leftover body")
	}
	if _, err := os.Stat(filepath.Join(tsDir, "stray.project.db")); err != nil {
		t.Fatalf("sibling .db not under the same <ts> dir: %v", err)
	}

	// lostfound.quarantined emitted for ns/stray, path = project name.
	ev := quarantineEventFor(center, "ns", "stray")
	if ev == nil {
		t.Fatal("no lostfound.quarantined event for ns/stray")
	}
	if ev.Path != "stray" {
		t.Fatalf("event path = %q, want stray", ev.Path)
	}
}

// TestRelocateLeftovers_DirOnlyGraceful: a leftover with NO sibling .db relocates the
// directory alone, without error, and still emits the NOTIFY (§3.2 graceful missing
// .db).
func TestRelocateLeftovers_DirOnlyGraceful(t *testing.T) {
	center := notify.NewCenter(16)
	s := newStorageWithCenter(t, center)
	lf := makeLeftover(t, s, "ns", "stray", false) // dbPath == ""

	s.relocateLeftovers([]leftover{lf}, fixedTS)

	if _, err := os.Stat(lf.treePath); !os.IsNotExist(err) {
		t.Fatalf("leftover tree still present, stat err=%v", err)
	}
	tsDir := filepath.Join(s.lostFoundRoot("ns", "stray"), fixedTS.UTC().Format(lostFoundTimeFormat))
	if _, err := os.Stat(filepath.Join(tsDir, "stray", "directives", "x.md")); err != nil {
		t.Fatalf("relocated dir-only tree missing: %v", err)
	}
	if quarantineEventFor(center, "ns", "stray") == nil {
		t.Fatal("no lostfound.quarantined event for the dir-only relocation")
	}
}

// TestRelocateLeftovers_IdempotentWhenTreeGone: a leftover whose tree is already gone
// (relocated on a prior boot, or a crash-interrupted move) is skipped silently — no
// error, no NOTIFY, nothing created (§6 idempotency).
func TestRelocateLeftovers_IdempotentWhenTreeGone(t *testing.T) {
	center := notify.NewCenter(16)
	s := newStorageWithCenter(t, center)

	// A descriptor pointing at a tree that does not exist.
	lf := leftover{
		namespace: "ns",
		name:      "stray",
		treePath:  filepath.Join(s.baseDir, "ns", "stray"),
		dbPaths:   []string{s.catalogPath("ns", "stray")},
	}

	s.relocateLeftovers([]leftover{lf}, fixedTS)

	if quarantineEventFor(center, "ns", "stray") != nil {
		t.Fatal("a vanished leftover must not emit a quarantine event")
	}
	// No lost+found <ts> dir was created for the absent tree.
	tsDir := filepath.Join(s.lostFoundRoot("ns", "stray"), fixedTS.UTC().Format(lostFoundTimeFormat))
	if _, err := os.Stat(tsDir); !os.IsNotExist(err) {
		t.Fatalf("a quarantine dir was created for an absent leftover: stat err=%v", err)
	}
}

// TestStartupInit_RelocatesLeftover: end-to-end wiring. A repo-less leftover present
// at boot is quarantined by the non-blocking post-StartupInit step while the real
// project is registered normally. The relocation runs in a goroutine, so the test
// waits (bounded poll) for the quarantine NOTIFY rather than asserting a strict
// gate-returns-before-move ordering (documented, not raced — §5.2 / baseline §5.2).
func TestStartupInit_RelocatesLeftover(t *testing.T) {
	center := notify.NewCenter(64)
	s := newStorageWithCenter(t, center) // real git project ns/proj
	lf := makeLeftover(t, s, "ns", "stray", true)

	s.StartupInit(context.Background())

	// The synchronous gate returned; the real project is registered and healthy.
	if st := s.State("ns", "proj"); st != StateHealthy {
		t.Fatalf("real project state = %v, want healthy", st)
	}
	// The leftover was never registered as a project.
	if st := s.State("ns", "stray"); st == StateDangerous {
		t.Fatalf("leftover ns/stray was adopted and marked dangerous (state=%v); it must be relocated, not registered", st)
	}

	// Deterministically await the non-blocking relocation goroutine (B-42): once
	// relocWG drains, depositTree has fully returned and the quarantine NOTIFY has
	// been emitted. Replaces the former bounded poll — no timing, no t.TempDir()
	// teardown race.
	s.relocWG.Wait()
	if quarantineEventFor(center, "ns", "stray") == nil {
		t.Fatal("leftover was not relocated to lost+found (no quarantine NOTIFY)")
	}

	// Source tree and .db are gone after relocation.
	if _, err := os.Stat(lf.treePath); !os.IsNotExist(err) {
		t.Fatalf("leftover tree still present after relocation, stat err=%v", err)
	}
	if _, err := os.Stat(lf.dbPaths[0]); !os.IsNotExist(err) {
		t.Fatalf("leftover .db still present after relocation, stat err=%v", err)
	}
}

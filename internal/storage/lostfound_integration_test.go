package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shoka/mcp-server/internal/notify"
)

// TestLostFoundWorker_EndToEnd exercises the full worker path through the real
// periodic sweep and NOTIFY dispatch to a subscriber: a disposable file is
// deleted, an unknown file is preserved in lost+found, a tracked file is
// untouched, and both worker events reach a subscriber.
func TestLostFoundWorker_EndToEnd(t *testing.T) {
	s, c := newWorkerStorage(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One committed, tracked file.
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "keep", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// Shoka-wide disposable pattern for OS junk.
	writeDisposable(t, filepath.Join(s.baseDir, "shoka.disposable"), ".DS_Store")

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	// An untracked disposable file and an untracked unknown file.
	if err := os.WriteFile(filepath.Join(projectPath, ".DS_Store"), []byte("\x00junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "mystery.md"), []byte("unknown"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Subscribe before starting the worker; collect worker events.
	events := make(chan notify.Event, 64)
	unsub := c.Subscribe(func(e notify.Event) {
		switch e.Kind {
		case "lostfound.moved", "lostfound.disposed":
			select {
			case events <- e:
			default:
			}
		}
	})
	defer unsub()

	// Start the real periodic worker.
	s.StartLostFoundSweep(ctx, 5*time.Millisecond)

	var disposed, moved *notify.Event
	deadline := time.After(2 * time.Second)
	for disposed == nil || moved == nil {
		select {
		case e := <-events:
			ev := e
			if ev.Kind == "lostfound.disposed" && ev.Path == ".DS_Store" {
				disposed = &ev
			}
			if ev.Kind == "lostfound.moved" && ev.Path == "mystery.md" {
				moved = &ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for worker events (disposed=%v moved=%v)", disposed, moved)
		}
	}

	// Disposable deleted, unknown preserved, tracked untouched.
	if _, err := os.Stat(filepath.Join(projectPath, ".DS_Store")); !os.IsNotExist(err) {
		t.Fatalf(".DS_Store should be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "mystery.md")); !os.IsNotExist(err) {
		t.Fatalf("mystery.md should be moved out of the working tree, stat err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(projectPath, "real.md")); err != nil || string(got) != "keep" {
		t.Fatalf("tracked real.md must be untouched: content=%q err=%v", got, err)
	}
	// mystery.md preserved in the lost+found area.
	found := false
	_ = filepath.WalkDir(s.lostFoundRoot("ns", "proj"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == "mystery.md" {
			if b, _ := os.ReadFile(p); string(b) == "unknown" {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Fatal("mystery.md not preserved in lost+found area with original content")
	}
	if disposed.Target != "ns/proj" || moved.Target != "ns/proj" {
		t.Fatalf("events should target ns/proj: disposed=%+v moved=%+v", *disposed, *moved)
	}
}

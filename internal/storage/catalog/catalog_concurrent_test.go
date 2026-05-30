package catalog

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentPutGetList exercises the catalog under many goroutines doing
// mixed Put/Get/List on distinct paths. bbolt serialises writers and allows
// concurrent readers; this asserts no panics, no data races (run under -race),
// and that every written entry is readable afterward.
func TestConcurrentPutGetList(t *testing.T) {
	c := newCatalog(t)
	const writers = 20
	const perWriter = 50

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				p := fmt.Sprintf("dir%d/file%d.md", w, i)
				if err := c.PutFile(p, entry(p)); err != nil {
					t.Errorf("PutFile(%s): %v", p, err)
					return
				}
				if _, ok, err := c.GetFile(p); err != nil || !ok {
					t.Errorf("GetFile(%s): ok=%v err=%v", p, ok, err)
					return
				}
				if _, _, err := c.List(fmt.Sprintf("dir%d", w)); err != nil {
					t.Errorf("List(dir%d): %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	st, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.FileCount != writers*perWriter {
		t.Errorf("FileCount = %d, want %d", st.FileCount, writers*perWriter)
	}
}

// TestConcurrentWriteDeleteSamePath hammers a single path with interleaved
// writes and deletes from many goroutines. The only guarantee is no panic / no
// race and a self-consistent terminal state (present-and-readable or absent).
func TestConcurrentWriteDeleteSamePath(t *testing.T) {
	c := newCatalog(t)
	const goroutines = 30
	const iters = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if (g+i)%2 == 0 {
					if err := c.PutFile("hot.md", entry("v")); err != nil {
						t.Errorf("PutFile: %v", err)
						return
					}
				} else {
					if err := c.DeleteFile("hot.md"); err != nil {
						t.Errorf("DeleteFile: %v", err)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()

	// Terminal state must be self-consistent: GetFile agrees with HasFile.
	_, ok, err := c.GetFile("hot.md")
	if err != nil {
		t.Fatal(err)
	}
	has, err := c.HasFile("hot.md")
	if err != nil {
		t.Fatal(err)
	}
	if ok != has {
		t.Errorf("GetFile ok=%v but HasFile=%v (inconsistent)", ok, has)
	}
}

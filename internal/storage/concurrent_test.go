package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectWTRoot is the working-tree root of a project under test.
func projectWTRoot(s *FSGitStorage, ns, proj string) string {
	return filepath.Join(s.baseDir, ns, proj)
}

// assertNoViolations fails if the project's catalog invariant does not hold.
func assertNoViolations(t *testing.T, s *FSGitStorage, ns, proj string) {
	t.Helper()
	cat, err := s.catalogFor(ns, proj)
	require.NoError(t, err)
	v, err := cat.VerifyInvariant(projectWTRoot(s, ns, proj))
	require.NoError(t, err)
	assert.Empty(t, v, "catalog invariant must hold for %s/%s: %+v", ns, proj, v)
}

func makeProjectStore(t *testing.T, projects ...string) *FSGitStorage {
	t.Helper()
	s, _ := newStore(t, Options{})
	for _, p := range projects {
		require.NoError(t, s.CreateProject("ns", p))
	}
	return s
}

// test-1: N goroutines write the SAME path with the SAME initial if_match.
// Exactly one wins; the rest get etag conflicts. The catalog and working tree
// agree on the winner, and the invariant holds.
func TestConcurrent_SameFileWrites(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	const writers = 20
	emptyEtag := contentSHA("") // the file does not exist yet

	var success, conflict atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ifm := emptyEtag
			_, err := s.Write(ctx, "sess", "ns", "proj", "hot.md", fmt.Sprintf("content-%d", i), &ifm)
			if err == nil {
				success.Add(1)
				return
			}
			var conf *VersionConflictError
			require.ErrorAs(t, err, &conf, "non-winning writes must be etag conflicts")
			conflict.Add(1)
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int64(1), success.Load(), "exactly one writer wins")
	assert.Equal(t, int64(writers-1), conflict.Load(), "the rest conflict")

	// Catalog entry matches the winning working-tree content.
	content, etag, err := s.ReadFileWithETag("ns", "proj", "hot.md")
	require.NoError(t, err)
	cat, err := s.catalogFor("ns", "proj")
	require.NoError(t, err)
	entry, ok, err := cat.GetFile("hot.md")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, etag, entry.Etag, "catalog etag must match the working-tree content")
	assert.Equal(t, contentSHA(content), entry.Etag)
	assertNoViolations(t, s, "ns", "proj")
}

// test-2: distinct paths in one project, all concurrent, all succeed.
func TestConcurrent_DistinctPathsSameProject(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	const n = 20

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Write(ctx, "sess", "ns", "proj", fmt.Sprintf("f%02d.md", i), fmt.Sprintf("v%d", i), nil)
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	cat, err := s.catalogFor("ns", "proj")
	require.NoError(t, err)
	st, err := cat.Stats()
	require.NoError(t, err)
	assert.Equal(t, n, st.FileCount, "every distinct write must be in the catalog")
	assertNoViolations(t, s, "ns", "proj")
}

// test-3: 4 projects, 5 goroutines each, distinct paths; all consistent.
func TestConcurrent_CrossProject(t *testing.T) {
	projects := []string{"p1", "p2", "p3", "p4"}
	s := makeProjectStore(t, projects...)
	ctx := context.Background()
	const perProject = 5

	var wg sync.WaitGroup
	for _, p := range projects {
		for i := 0; i < perProject; i++ {
			wg.Add(1)
			go func(p string, i int) {
				defer wg.Done()
				_, err := s.Write(ctx, "sess", "ns", p, fmt.Sprintf("f%d.md", i), "x", nil)
				assert.NoError(t, err)
			}(p, i)
		}
	}
	wg.Wait()

	for _, p := range projects {
		cat, err := s.catalogFor("ns", p)
		require.NoError(t, err)
		st, _ := cat.Stats()
		assert.Equal(t, perProject, st.FileCount, "project %s file count", p)
		assertNoViolations(t, s, "ns", p)
	}
}

// test-4: write and delete racing on the same path. Both run under the same
// per-file lock, so the last to complete sets a consistent (file+catalog) state.
func TestConcurrent_WriteDeleteSamePath(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	const rounds = 50

	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.Write(ctx, "sess", "ns", "proj", "x.md", "v", nil) }()
		go func() { defer wg.Done(); _ = s.Delete(ctx, "sess", "ns", "proj", "x.md", nil) }()
	}
	wg.Wait()

	// Working-tree presence must agree with catalog presence; the invariant holds.
	cat, err := s.catalogFor("ns", "proj")
	require.NoError(t, err)
	inCatalog, err := cat.HasFile("x.md")
	require.NoError(t, err)
	_, _, readErr := s.ReadFileWithETag("ns", "proj", "x.md")
	onDisk := readErr == nil
	assert.Equal(t, inCatalog, onDisk, "catalog presence must match working-tree presence")
	assertNoViolations(t, s, "ns", "proj")
}

// test-5: writes racing with list_files. Every listing is internally consistent
// and every listed entry has a working-tree file.
func TestConcurrent_WriteListRace(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	const n = 30

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			_, err := s.Write(ctx, "sess", "ns", "proj", fmt.Sprintf("f%02d.md", i), "v", nil)
			assert.NoError(t, err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n*3; i++ {
			files, modTimes, err := s.ListFiles("ns", "proj", "")
			assert.NoError(t, err)
			for _, f := range files {
				_, ok := modTimes[f]
				assert.True(t, ok, "every listed entry has a modified_at")
			}
		}
	}()
	wg.Wait()
	assertNoViolations(t, s, "ns", "proj")
}

// test-6: writes racing with reads. No torn content: every successful read
// returns one of the values that was actually written.
func TestConcurrent_WriteReadRace(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	valid := map[string]bool{"alpha": true, "bravo": true, "charlie": true}
	values := []string{"alpha", "bravo", "charlie"}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			_, err := s.Write(ctx, "sess", "ns", "proj", "shared.md", values[i%len(values)], nil)
			assert.NoError(t, err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 180; i++ {
			content, _, err := s.ReadFileWithETag("ns", "proj", "shared.md")
			if err != nil {
				continue // legitimately not yet written
			}
			assert.True(t, valid[content], "read returned non-torn content: %q", content)
		}
	}()
	wg.Wait()
	assertNoViolations(t, s, "ns", "proj")
}

// test-7: a capped mixed-operation stress over distinct per-goroutine paths.
// Asserts no deadlock (every goroutine completes), no panic, and a clean
// invariant at the end. Runs under -race.
func TestConcurrent_MixedStress(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	const goroutines = 50
	const ops = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				path := fmt.Sprintf("g%02d/f%03d.md", g, i%20)
				switch i % 4 {
				case 0, 1:
					_, _ = s.Write(ctx, "sess", "ns", "proj", path, fmt.Sprintf("v%d", i), nil)
				case 2:
					_, _, _ = s.ReadFileWithETag("ns", "proj", path)
				case 3:
					_ = s.Delete(ctx, "sess", "ns", "proj", path, nil)
				}
			}
		}(g)
	}
	wg.Wait()
	assertNoViolations(t, s, "ns", "proj")
}

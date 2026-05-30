package wal

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func walDir(base string) string       { return filepath.Join(base, ".shoka", "wal") }
func corruptedDir(base string) string { return filepath.Join(base, ".shoka", "wal-corrupted") }

func TestWAL_RoundTrip(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	content := []byte("# Title\n\nbody bytes")
	seq, err := l.Append(Entry{Namespace: "ns", Project: "proj", Path: "a.md", Op: "write", Content: content})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), seq)
	assert.Equal(t, 1, l.PendingCount())
	assert.Equal(t, int64(len(content)), l.PendingBytes())

	heads, err := l.ListPending()
	require.NoError(t, err)
	require.Len(t, heads, 1)
	assert.Equal(t, int64(1), heads[0].Seq)
	assert.Equal(t, "write", heads[0].Op)
	assert.Equal(t, "a.md", heads[0].Path)
	assert.Equal(t, int64(len(content)), heads[0].Size)

	got, err := l.ReadByID(1)
	require.NoError(t, err)
	assert.Equal(t, content, got.Content)
	assert.Equal(t, "ns", got.Namespace)
	assert.Equal(t, "proj", got.Project)
	wantVer := hex.EncodeToString(func() []byte { s := sha256.Sum256(content); return s[:] }())
	assert.Equal(t, wantVer, got.Version)

	require.NoError(t, l.Remove(1))
	assert.Equal(t, 0, l.PendingCount())
	assert.Equal(t, int64(0), l.PendingBytes())
}

func TestWAL_DeleteEntryShape(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)

	seq, err := l.Append(Entry{Namespace: "ns", Project: "p", Path: "gone.md", Op: "delete"})
	require.NoError(t, err)

	got, err := l.ReadByID(seq)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got.Size)
	assert.Empty(t, got.Content)
	// SHA-256 of empty content.
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", got.Version)
}

func TestWAL_ConcurrentAppend(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)

	const n = 200
	var wg sync.WaitGroup
	seqs := make([]uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := l.Append(Entry{Namespace: "ns", Project: "p", Path: fmt.Sprintf("f%d.md", i), Op: "write", Content: []byte(fmt.Sprintf("data-%d", i))})
			assert.NoError(t, err)
			seqs[i] = s
		}(i)
	}
	wg.Wait()

	assert.Equal(t, n, l.PendingCount())
	// All seqs unique and within [1, n].
	seen := make(map[uint64]bool, n)
	for _, s := range seqs {
		assert.False(t, seen[s], "duplicate seq %d", s)
		seen[s] = true
		assert.GreaterOrEqual(t, s, uint64(1))
		assert.LessOrEqual(t, s, uint64(n))
	}
}

func TestWAL_AtomicRenameIgnoresTempLeftover(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)
	_, err = l.Append(Entry{Namespace: "ns", Project: "p", Path: "a.md", Op: "write", Content: []byte("x")})
	require.NoError(t, err)
	require.NoError(t, l.Close())

	// Simulate a crash mid-write: a temp file that was never renamed.
	tmpLeftover := filepath.Join(walDir(base), ".tmp", seqName(99))
	require.NoError(t, os.WriteFile(tmpLeftover, []byte("partial garbage"), 0o644))

	l2, err := Open(base)
	require.NoError(t, err)
	assert.Equal(t, 1, l2.PendingCount(), "temp leftover must be ignored")
	// Next seq must continue from the real max (1), not the leftover (99).
	seq, err := l2.Append(Entry{Namespace: "ns", Project: "p", Path: "b.md", Op: "write", Content: []byte("y")})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), seq)
}

func TestWAL_CorruptionQuarantinedOnOpen(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	// Hand-craft an entry whose version does not match its content.
	w := wireEntry{
		Seq:        seqHex(1),
		Ts:         time.Now().UTC().Format(time.RFC3339Nano),
		Namespace:  "ns",
		Project:    "p",
		Path:       "bad.md",
		Op:         "write",
		Version:    "deadbeef", // wrong
		Size:       5,
		ContentB64: base64.StdEncoding.EncodeToString([]byte("hello")),
	}
	data, _ := json.Marshal(w)
	require.NoError(t, os.WriteFile(filepath.Join(walDir(base), seqName(1)), data, 0o644))

	l2, err := Open(base)
	require.NoError(t, err)
	assert.Equal(t, 0, l2.PendingCount(), "corrupt entry must not be pending")

	// The file should have been moved to wal-corrupted/.
	_, statErr := os.Stat(filepath.Join(walDir(base), seqName(1)))
	assert.True(t, os.IsNotExist(statErr), "corrupt entry must be removed from wal/")
	_, statErr = os.Stat(filepath.Join(corruptedDir(base), seqName(1)))
	assert.NoError(t, statErr, "corrupt entry must be in wal-corrupted/")
}

func TestWAL_SequenceRecoveryWithGap(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)
	// Write 1,2,3,5 (skip 4) by appending 5 then removing #4.
	for i := 0; i < 5; i++ {
		_, err := l.Append(Entry{Namespace: "ns", Project: "p", Path: fmt.Sprintf("f%d", i), Op: "write", Content: []byte("c")})
		require.NoError(t, err)
	}
	require.NoError(t, l.Remove(4))
	require.NoError(t, l.Close())

	l2, err := Open(base)
	require.NoError(t, err)
	assert.Equal(t, 4, l2.PendingCount())
	seq, err := l2.Append(Entry{Namespace: "ns", Project: "p", Path: "next", Op: "write", Content: []byte("c")})
	require.NoError(t, err)
	assert.Equal(t, uint64(6), seq, "next seq must be max(5)+1")
}

func TestWAL_OldestEntryAge(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), l.OldestEntryAge(), "no entries => zero")

	_, err = l.Append(Entry{Namespace: "ns", Project: "p", Path: "old.md", Op: "write", Content: []byte("c")})
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)
	age := l.OldestEntryAge()
	assert.GreaterOrEqual(t, age, 30*time.Millisecond)

	// Removing the only entry returns to zero.
	require.NoError(t, l.Remove(1))
	assert.Equal(t, time.Duration(0), l.OldestEntryAge())
}

func TestWAL_ReadByID_QuarantinesCorruptOnRead(t *testing.T) {
	base := t.TempDir()
	l, err := Open(base)
	require.NoError(t, err)
	seq, err := l.Append(Entry{Namespace: "ns", Project: "p", Path: "a.md", Op: "write", Content: []byte("hello")})
	require.NoError(t, err)

	// Corrupt the on-disk file after it is pending.
	require.NoError(t, os.WriteFile(filepath.Join(walDir(base), seqName(seq)), []byte("{not valid json"), 0o644))

	_, err = l.ReadByID(seq)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(corruptedDir(base), seqName(seq)))
	assert.NoError(t, statErr, "corrupt-on-read entry should be quarantined")
	assert.Equal(t, 0, l.PendingCount(), "and dropped from the pending index")
}

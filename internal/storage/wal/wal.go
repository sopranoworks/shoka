// Package wal is Shoka's persistent write-ahead log. Every write/delete that has
// landed on the working tree (layer 1) is appended here (layer 2) before the
// background worker commits it to git (layer 3). The WAL is what makes the file
// system and git safely asynchronous: if Shoka crashes between writing a file
// and committing it, the entry survives and the worker resumes on restart.
//
// On-disk layout (rooted at <base_dir>/.shoka/wal/):
//
//	<base_dir>/.shoka/wal/0000000000000001.json   one file per pending entry
//	<base_dir>/.shoka/wal/.tmp/                    staging for atomic rename
//	<base_dir>/.shoka/wal-corrupted/               quarantined integrity failures
//
// One file per entry (rather than one appended log) is trivially robust to
// crashes (atomic os.Rename, no partial-line problem), observable (ls), and
// garbage-collected (delete the file). Every field is always present and the
// format is uniform across write and delete.
package wal

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is a single WAL record. Content is the decoded bytes; the on-disk form
// stores it base64-encoded. Version is the SHA-256 hex of Content (not a git
// hash). For a delete, Content is empty, Size is 0, and Version is the SHA-256
// of the empty byte slice.
type Entry struct {
	Seq       uint64
	Ts        time.Time
	Namespace string
	Project   string
	Path      string
	Op        string // "write" or "delete"
	Version   string // sha256 hex of content
	Size      int64
	Content   []byte

	// Commit-author identity (the 2026-06-01 identity-config directive). Carried
	// here because the commit is produced asynchronously by the WAL worker, so
	// the identity must survive a restart mid-drain. Empty on entries written
	// before identity existed; the worker fills those from the configured
	// default. PROVISIONAL — see internal/identity (backlog B-28).
	UserName  string
	UserEmail string
	AgentName string
	WorkerID  string

	// AuthorIsUser records that the git Author should be the owning user rather
	// than the agent (the web /ws/ui SAVE_FILE path). Carried here so the async
	// commit worker honours it after a restart. Defaults to false (agent-author),
	// so entries written before this field decode to today's behaviour.
	AuthorIsUser bool
}

// EntryHead is an entry's metadata without its content, used by the dispatcher.
type EntryHead struct {
	Seq, Size                             int64
	Ts                                    time.Time
	Namespace, Project, Path, Op, Version string
}

// wireEntry is the exact on-disk JSON shape. seq is a 16-hex string; ts is
// RFC3339Nano; content is base64. Every field is always emitted.
type wireEntry struct {
	Seq        string `json:"seq"`
	Ts         string `json:"ts"`
	Namespace  string `json:"namespace"`
	Project    string `json:"project"`
	Path       string `json:"path"`
	Op         string `json:"op"`
	Version    string `json:"version"`
	Size       int64  `json:"size"`
	ContentB64 string `json:"content_b64"`
	// Identity fields, omitempty so older entries (and the integrity invariant)
	// are unaffected; absent fields decode to "" and the worker defaults them.
	UserName     string `json:"user_name,omitempty"`
	UserEmail    string `json:"user_email,omitempty"`
	AgentName    string `json:"agent_name,omitempty"`
	WorkerID     string `json:"worker_id,omitempty"`
	AuthorIsUser bool   `json:"author_is_user,omitempty"`
}

// Log is an open write-ahead log. It is safe for concurrent use.
type Log struct {
	dir          string // <base_dir>/.shoka/wal
	tmpDir       string // <base_dir>/.shoka/wal/.tmp
	corruptedDir string // <base_dir>/.shoka/wal-corrupted

	mu           sync.Mutex
	nextSeq      uint64
	pending      map[uint64]EntryHead
	pendingBytes int64

	logger *slog.Logger
}

// Open opens (or creates) the WAL rooted at baseDir/.shoka/wal/. It scans
// existing entries to recover the next seq number and moves any integrity-failed
// entries to baseDir/.shoka/wal-corrupted/.
func Open(baseDir string) (*Log, error) {
	dir := filepath.Join(baseDir, ".shoka", "wal")
	l := &Log{
		dir:          dir,
		tmpDir:       filepath.Join(dir, ".tmp"),
		corruptedDir: filepath.Join(baseDir, ".shoka", "wal-corrupted"),
		nextSeq:      1,
		pending:      make(map[uint64]EntryHead),
		logger:       slog.Default(),
	}
	if err := os.MkdirAll(l.tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: create dir: %w", err)
	}
	if err := l.scan(); err != nil {
		return nil, err
	}
	return l, nil
}

// scan reads the WAL directory, validating each entry and quarantining failures.
func (l *Log) scan() error {
	dirents, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("wal: read dir: %w", err)
	}
	var maxSeq uint64
	for _, de := range dirents {
		if de.IsDir() {
			continue // skip .tmp/
		}
		seq, ok := parseSeqName(de.Name())
		if !ok {
			continue // ignore non-entry files
		}
		full := filepath.Join(l.dir, de.Name())
		e, err := readEntryFile(full, seq)
		if err != nil {
			l.quarantine(full, de.Name(), err)
			continue
		}
		l.pending[seq] = headOf(e)
		l.pendingBytes += e.Size
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	l.nextSeq = maxSeq + 1
	return nil
}

// Close releases resources. WAL files on disk are not removed.
func (l *Log) Close() error { return nil }

// Append writes a new entry. Seq and Ts are assigned here (callers leave them
// zero); Version and Size are (re)computed from Content so the on-disk integrity
// invariant always holds. Returns the assigned seq.
func (l *Log) Append(e Entry) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	seq := l.nextSeq
	e.Seq = seq
	e.Ts = time.Now().UTC()
	if e.Content == nil {
		e.Content = []byte{}
	}
	e.Size = int64(len(e.Content))
	e.Version = sha256Hex(e.Content)

	data, err := json.Marshal(toWire(e))
	if err != nil {
		return 0, fmt.Errorf("wal: marshal entry: %w", err)
	}

	name := seqName(seq)
	tmp := filepath.Join(l.tmpDir, name)
	final := filepath.Join(l.dir, name)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return 0, fmt.Errorf("wal: write temp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("wal: rename: %w", err)
	}

	l.pending[seq] = headOf(e)
	l.pendingBytes += e.Size
	l.nextSeq++
	return seq, nil
}

// PendingCount returns the number of entries currently on disk. Backs the
// shoka_wal_pending_entries metric.
func (l *Log) PendingCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}

// PendingBytes returns the summed Size of pending entries. Backs the
// shoka_wal_pending_bytes metric. (Not in the directive's §5.2 surface list but
// required by the §11 metric; cached, refreshed on add/remove.)
func (l *Log) PendingBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pendingBytes
}

// ListPending returns all pending entries (without Content), sorted by seq.
func (l *Log) ListPending() ([]EntryHead, error) {
	l.mu.Lock()
	heads := make([]EntryHead, 0, len(l.pending))
	for _, h := range l.pending {
		heads = append(heads, h)
	}
	l.mu.Unlock()
	sort.Slice(heads, func(i, j int) bool { return heads[i].Seq < heads[j].Seq })
	return heads, nil
}

// ReadByID returns the full entry (Content decoded). A failed integrity check
// quarantines the file and returns an error.
func (l *Log) ReadByID(seq uint64) (Entry, error) {
	full := filepath.Join(l.dir, seqName(seq))
	e, err := readEntryFile(full, seq)
	if err != nil {
		l.quarantine(full, seqName(seq), err)
		l.mu.Lock()
		if _, ok := l.pending[seq]; ok {
			l.pendingBytes -= l.pending[seq].Size
			delete(l.pending, seq)
		}
		l.mu.Unlock()
		return Entry{}, err
	}
	return e, nil
}

// Remove deletes the entry from disk (idempotent if already gone). Called by the
// worker after a successful git commit.
func (l *Log) Remove(seq uint64) error {
	full := filepath.Join(l.dir, seqName(seq))
	err := os.Remove(full)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("wal: remove %d: %w", seq, err)
	}
	l.mu.Lock()
	if h, ok := l.pending[seq]; ok {
		l.pendingBytes -= h.Size
		delete(l.pending, seq)
	}
	l.mu.Unlock()
	return nil
}

// OldestEntryAge reports the age of the oldest pending entry, or zero if none.
// Backs the shoka_wal_oldest_entry_age_seconds metric.
func (l *Log) OldestEntryAge() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var oldest time.Time
	for _, h := range l.pending {
		if oldest.IsZero() || h.Ts.Before(oldest) {
			oldest = h.Ts
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}

// --- helpers ---

func (l *Log) quarantine(fullPath, name string, cause error) {
	if err := os.MkdirAll(l.corruptedDir, 0o755); err != nil {
		l.logger.Error("wal: cannot create quarantine dir", "error", err)
		return
	}
	dst := filepath.Join(l.corruptedDir, name)
	if err := os.Rename(fullPath, dst); err != nil {
		l.logger.Error("wal: failed to quarantine corrupt entry", "file", name, "cause", cause, "error", err)
		return
	}
	l.logger.Error("wal: quarantined corrupt entry", "file", name, "moved_to", dst, "cause", cause)
}

// readEntryFile reads, parses, and integrity-checks one entry file. wantSeq is
// the seq encoded in the filename; the on-disk seq must match it.
func readEntryFile(path string, wantSeq uint64) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("wal: read %s: %w", filepath.Base(path), err)
	}
	var w wireEntry
	if err := json.Unmarshal(data, &w); err != nil {
		return Entry{}, fmt.Errorf("wal: parse %s: %w", filepath.Base(path), err)
	}
	seq, err := strconv.ParseUint(strings.TrimSpace(w.Seq), 16, 64)
	if err != nil {
		return Entry{}, fmt.Errorf("wal: bad seq field %q: %w", w.Seq, err)
	}
	if seq != wantSeq {
		return Entry{}, fmt.Errorf("wal: seq mismatch: filename=%d body=%d", wantSeq, seq)
	}
	content, err := base64.StdEncoding.DecodeString(w.ContentB64)
	if err != nil {
		return Entry{}, fmt.Errorf("wal: bad base64 content: %w", err)
	}
	if int64(len(content)) != w.Size {
		return Entry{}, fmt.Errorf("wal: size mismatch: field=%d actual=%d", w.Size, len(content))
	}
	if got := sha256Hex(content); got != w.Version {
		return Entry{}, fmt.Errorf("wal: hash mismatch: field=%s actual=%s", w.Version, got)
	}
	ts, err := time.Parse(time.RFC3339Nano, w.Ts)
	if err != nil {
		return Entry{}, fmt.Errorf("wal: bad ts %q: %w", w.Ts, err)
	}
	return Entry{
		Seq:       seq,
		Ts:        ts,
		Namespace: w.Namespace,
		Project:   w.Project,
		Path:      w.Path,
		Op:        w.Op,
		Version:   w.Version,
		Size:      w.Size,
		Content:      content,
		UserName:     w.UserName,
		UserEmail:    w.UserEmail,
		AgentName:    w.AgentName,
		WorkerID:     w.WorkerID,
		AuthorIsUser: w.AuthorIsUser,
	}, nil
}

func toWire(e Entry) wireEntry {
	return wireEntry{
		Seq:        seqHex(e.Seq),
		Ts:         e.Ts.UTC().Format(time.RFC3339Nano),
		Namespace:  e.Namespace,
		Project:    e.Project,
		Path:       e.Path,
		Op:         e.Op,
		Version:    e.Version,
		Size:       e.Size,
		ContentB64:   base64.StdEncoding.EncodeToString(e.Content),
		UserName:     e.UserName,
		UserEmail:    e.UserEmail,
		AgentName:    e.AgentName,
		WorkerID:     e.WorkerID,
		AuthorIsUser: e.AuthorIsUser,
	}
}

func headOf(e Entry) EntryHead {
	return EntryHead{
		Seq:       int64(e.Seq),
		Size:      e.Size,
		Ts:        e.Ts,
		Namespace: e.Namespace,
		Project:   e.Project,
		Path:      e.Path,
		Op:        e.Op,
		Version:   e.Version,
	}
}

func seqHex(seq uint64) string  { return fmt.Sprintf("%016x", seq) }
func seqName(seq uint64) string { return seqHex(seq) + ".json" }

// parseSeqName extracts the seq from a "<16-hex>.json" filename.
func parseSeqName(name string) (uint64, bool) {
	if !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	base := strings.TrimSuffix(name, ".json")
	if len(base) != 16 {
		return 0, false
	}
	seq, err := strconv.ParseUint(base, 16, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

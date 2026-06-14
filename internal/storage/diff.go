package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Diff caps (directive §2.3). No clean config seam fits, so these live as
// package consts. Every cap is surfaced via FileDiff.Suppressed — the content
// is never silently truncated.
const (
	// maxDiffInputBytes is the per-side blob byte cap; either side larger than
	// this skips the diff with Suppressed="too_large".
	maxDiffInputBytes = 512 * 1024
	// maxDiffLines bounds the total emitted lines; overflow sets
	// Suppressed="too_large".
	maxDiffLines = 20000
	// diffTimeout bounds the heavy diff.Do work via context cancellation
	// (go-git honours ctx mid-diff); a hit sets Suppressed="timeout".
	diffTimeout = 5 * time.Second
	// diffContextLines is the number of unchanged lines kept around each change
	// (git's default).
	diffContextLines = 3
)

// DiffVersions computes the structured diff of path between two explicit,
// immutable commits. It mirrors ReadFileAtVersion's lock-free model exactly: an
// independent PlainOpen handle, two explicit commit objects, and content-
// addressed immutable tree/blob reads — NO lock (no s.locks.WithLock, no
// mutex), no HEAD/index/working-tree resolution, no ref write. The "no lock"
// property is structural: the inputs are immutable objects, so nothing the
// background commit worker does can change them mid-read.
func (s *FSGitStorage) DiffVersions(ctx context.Context, namespace, projectName, path, fromHash, toHash string) (FileDiff, error) {
	fd := FileDiff{Path: path, FromHash: fromHash, ToHash: toHash}

	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return FileDiff{}, err
	}
	_, rel, err := relWithin(projectPath, path)
	if err != nil {
		return FileDiff{}, err
	}

	// Independent handle, no lock — same as GetHistory / ReadFileAtVersion.
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return FileDiff{}, fmt.Errorf("failed to open git repository: %w", err)
	}

	// Two EXPLICIT immutable commits (no implicit "diff against current"). A
	// bad/unknown hash is a clean typed error, never a panic.
	fromCommit, err := r.CommitObject(plumbing.NewHash(fromHash))
	if err != nil {
		return FileDiff{}, fmt.Errorf("failed to get 'from' commit %q: %w", fromHash, err)
	}
	toCommit, err := r.CommitObject(plumbing.NewHash(toHash))
	if err != nil {
		return FileDiff{}, fmt.Errorf("failed to get 'to' commit %q: %w", toHash, err)
	}
	fromTree, err := fromCommit.Tree()
	if err != nil {
		return FileDiff{}, fmt.Errorf("failed to get 'from' tree: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return FileDiff{}, fmt.Errorf("failed to get 'to' tree: %w", err)
	}

	// Resolve the file's entry on each side; absence on a side is add/delete.
	fromEntry, fromErr := fromTree.FindEntry(rel)
	toEntry, toErr := toTree.FindEntry(rel)
	fromPresent := fromErr == nil
	toPresent := toErr == nil

	switch {
	case !fromPresent && !toPresent:
		return FileDiff{}, fmt.Errorf("file %q not present in either commit", path)
	case !fromPresent:
		fd.Status = "added"
	case !toPresent:
		fd.Status = "deleted"
	default:
		fd.Status = "modified"
	}

	// Binary short-circuit and per-side byte cap, both before any heavy diff.
	// IsBinary sniffs only a prefix and Size comes from the blob header, so
	// neither reads the full content.
	for _, side := range []struct {
		tree    *object.Tree
		entry   *object.TreeEntry
		present bool
	}{{fromTree, fromEntry, fromPresent}, {toTree, toEntry, toPresent}} {
		if !side.present {
			continue
		}
		f, err := side.tree.TreeEntryFile(side.entry)
		if err != nil {
			return FileDiff{}, fmt.Errorf("failed to read blob for %q: %w", path, err)
		}
		bin, err := f.IsBinary()
		if err != nil {
			return FileDiff{}, fmt.Errorf("failed to classify %q: %w", path, err)
		}
		if bin {
			fd.Binary = true
			fd.Suppressed = "binary"
			return fd, nil
		}
		if f.Size > maxDiffInputBytes {
			fd.Suppressed = "too_large"
			return fd, nil
		}
	}

	// Compute the single-file patch under a deadline. Change.PatchContext (not
	// Tree.PatchContext) scopes the diff to exactly this path — go-git's diff
	// reads only immutable objects, honours ctx mid-diff (the time-cap), and the
	// computation takes no lock.
	cctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	change := &object.Change{}
	if fromPresent {
		change.From = object.ChangeEntry{Name: rel, Tree: fromTree, TreeEntry: *fromEntry}
	}
	if toPresent {
		change.To = object.ChangeEntry{Name: rel, Tree: toTree, TreeEntry: *toEntry}
	}

	patch, err := change.PatchContext(cctx)
	if err != nil {
		if cctx.Err() != nil || errors.Is(err, object.ErrCanceled) {
			fd.Suppressed = "timeout"
			return fd, nil
		}
		return FileDiff{}, fmt.Errorf("failed to compute diff for %q: %w", path, err)
	}

	fps := patch.FilePatches()
	if len(fps) == 0 {
		return fd, nil // nothing to diff (e.g. both sides empty)
	}

	hunks, suppressed := chunksToHunks(fps[0].Chunks())
	if suppressed != "" {
		fd.Suppressed = suppressed
		return fd, nil
	}
	fd.Hunks = hunks
	return fd, nil
}

// numberedLine is a single diff line tagged with its op and 1-based line number
// on each side (0 = not present on that side).
type numberedLine struct {
	op    string // "equal" | "add" | "delete"
	text  string
	oldLn int
	newLn int
}

// chunksToHunks converts go-git's line-oriented (op, text-run) chunks into
// context-collapsed, line-numbered hunks. diff.Do is line-oriented
// (DiffLinesToRunes), so each chunk is a whole number of lines. Returns a
// "too_large" suppression reason if the total line count exceeds maxDiffLines.
func chunksToHunks(chunks []fdiff.Chunk) ([]DiffHunk, string) {
	var flat []numberedLine
	oldLn, newLn := 1, 1
	total := 0
	for _, ch := range chunks {
		var op string
		switch ch.Type() {
		case fdiff.Equal:
			op = "equal"
		case fdiff.Add:
			op = "add"
		case fdiff.Delete:
			op = "delete"
		default:
			continue
		}
		for _, line := range splitDiffLines(ch.Content()) {
			total++
			if total > maxDiffLines {
				return nil, "too_large"
			}
			nl := numberedLine{op: op, text: strings.TrimSuffix(line, "\n")}
			switch op {
			case "equal":
				nl.oldLn, nl.newLn = oldLn, newLn
				oldLn++
				newLn++
			case "delete":
				nl.oldLn = oldLn
				oldLn++
			case "add":
				nl.newLn = newLn
				newLn++
			}
			flat = append(flat, nl)
		}
	}

	// Indices of changed (non-equal) lines.
	var changed []int
	for i, l := range flat {
		if l.op != "equal" {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil, "" // no changes (identical content)
	}

	var hunks []DiffHunk
	for i := 0; i < len(changed); {
		// Merge clusters separated by <= 2*ctx unchanged lines (unified-diff
		// semantics: their context windows would touch/overlap).
		j := i
		for j+1 < len(changed) && changed[j+1]-changed[j]-1 <= 2*diffContextLines {
			j++
		}
		start := changed[i] - diffContextLines
		if start < 0 {
			start = 0
		}
		end := changed[j] + diffContextLines
		if end > len(flat)-1 {
			end = len(flat) - 1
		}
		hunks = append(hunks, makeHunk(flat[start:end+1]))
		i = j + 1
	}
	return hunks, ""
}

// makeHunk builds a DiffHunk from a contiguous slice of numbered lines,
// deriving 1-based start lines and per-side counts. A pure-add hunk has
// OldStart/OldLines 0; a pure-delete hunk has NewStart/NewLines 0.
func makeHunk(lines []numberedLine) DiffHunk {
	var h DiffHunk
	for _, l := range lines {
		if l.oldLn > 0 {
			if h.OldStart == 0 {
				h.OldStart = l.oldLn
			}
			h.OldLines++
		}
		if l.newLn > 0 {
			if h.NewStart == 0 {
				h.NewStart = l.newLn
			}
			h.NewLines++
		}
		h.Lines = append(h.Lines, DiffLine{Op: l.op, Text: l.text})
	}
	return h
}

// diffLineSplit matches each line including its trailing newline (or the final
// line with no newline) — mirrors go-git's unified encoder splitLines.
var diffLineSplit = regexp.MustCompile(`[^\n]*(\n|$)`)

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	out := diffLineSplit.FindAllString(s, -1)
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

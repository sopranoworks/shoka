package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// Partial-edit primitives (backlog B-36): append_to_file and patch_file let an
// agent change a large append-mostly file (backlog.md, journal.md) by sending
// only the changed span instead of re-uploading the whole document through an
// LLM-mediated write_file. The splice always happens HERE, server-side, on the
// server's faithful bytes under the per-file lock — so the only LLM-mediated
// bytes are the small fragment(s) the caller passes. This shrinks the byte
// surface exposed to B-16 homoglyph substitution from the whole file to the
// edited span (range reduction, NOT a fidelity guarantee — the substitution, if
// any, happens in the model before the call and no server check can see it).
//
// Both ops route through the SAME write path as write_file via writeTransformed:
// the per-file lock, the if_match etag check, the atomic write, one ordinary
// "write" WAL entry (one faithful commit), the catalog update, and the file.write
// NOTIFY. The only new code is the byte computation below.

// MatchError is returned when a required unique span did not match exactly once.
// Count is the number of matches found: 0 (not found) or ≥2 (ambiguous). What is
// the offending argument name ("old_string" for patch_file, "anchor" for
// append_to_file). The server NEVER guesses which of several matches was meant —
// the caller must pass enough surrounding context to make the span unique.
type MatchError struct {
	What  string // "old_string" | "anchor"
	Count int
}

func (e *MatchError) Error() string {
	if e.Count == 0 {
		return fmt.Sprintf("%s not found in file", e.What)
	}
	return fmt.Sprintf("%s is ambiguous: %d matches; include more surrounding context to make it unique", e.What, e.Count)
}

// Validation (caller-input) errors for the partial-edit byte computation. These
// are bad arguments, distinct from MatchError (a content mismatch) and from the
// write-path's VersionConflictError / project-state errors.
var (
	// ErrFileNotFound means append_to_file targeted a path that does not exist.
	// append_to_file edits an EXISTING file; bringing a new file into existence is
	// write_file's job (which enforces the format allowlist). So an append to an
	// absent path is a typed error that creates nothing — closing the second
	// creation path that would otherwise bypass the allowlist (2026-06-22 fix).
	ErrFileNotFound = errors.New("file not found")
	// ErrEmptyOldString means patch_file was called with an empty old_string.
	// An empty needle has no well-defined single match, so it is rejected.
	ErrEmptyOldString = errors.New("old_string must not be empty")
	// ErrAnchorRequired means append_to_file used position before/after without an anchor.
	ErrAnchorRequired = errors.New("anchor is required when position is 'before' or 'after'")
	// ErrAnchorWithEnd means append_to_file used position:end with a (rejected) anchor.
	ErrAnchorWithEnd = errors.New("anchor must not be set when position is 'end'")
	// ErrInvalidPosition means append_to_file got a position other than end/before/after.
	ErrInvalidPosition = errors.New("invalid position: must be 'end', 'before', or 'after'")
)

// splicePatch performs the str_replace-style byte computation for patch_file:
// oldString must occur in cur exactly once; that one occurrence is replaced with
// newString. Zero matches → MatchError{Count:0}; two or more → MatchError{Count:N}.
// newString may be empty (a unique-span deletion). The result is returned; cur is
// not mutated.
func splicePatch(cur []byte, oldString, newString string) ([]byte, error) {
	if oldString == "" {
		return nil, ErrEmptyOldString
	}
	old := []byte(oldString)
	n := bytes.Count(cur, old)
	if n != 1 {
		return nil, &MatchError{What: "old_string", Count: n}
	}
	out := make([]byte, 0, len(cur)-len(old)+len(newString))
	idx := bytes.Index(cur, old)
	out = append(out, cur[:idx]...)
	out = append(out, newString...)
	out = append(out, cur[idx+len(old):]...)
	return out, nil
}

// spliceAppend performs the byte computation for append_to_file. position is
// "end" (default; empty string is treated as "end"), "before", or "after".
//   - end: content is appended verbatim at the end of cur (no separator added —
//     the caller owns newlines).
//   - before/after: anchor must occur exactly once; content is inserted verbatim
//     immediately before / after that single occurrence. Zero → MatchError{0};
//     ≥2 → MatchError{N}.
//
// anchor is required for before/after and rejected for end (the stricter choice:
// a misused anchor is a typed error, never a silent no-op). The result is
// returned; cur is not mutated.
func spliceAppend(cur, content []byte, position, anchor string) ([]byte, error) {
	switch position {
	case "", "end":
		if anchor != "" {
			return nil, ErrAnchorWithEnd
		}
		out := make([]byte, 0, len(cur)+len(content))
		out = append(out, cur...)
		out = append(out, content...)
		return out, nil
	case "before", "after":
		if anchor == "" {
			return nil, ErrAnchorRequired
		}
		a := []byte(anchor)
		n := bytes.Count(cur, a)
		if n != 1 {
			return nil, &MatchError{What: "anchor", Count: n}
		}
		idx := bytes.Index(cur, a)
		at := idx
		if position == "after" {
			at = idx + len(a)
		}
		out := make([]byte, 0, len(cur)+len(content))
		out = append(out, cur[:at]...)
		out = append(out, content...)
		out = append(out, cur[at:]...)
		return out, nil
	default:
		return nil, ErrInvalidPosition
	}
}

// AppendToFile inserts content into an EXISTING file and returns the new etag,
// reusing the write_file path verbatim via writeTransformed (per-file lock,
// if_match, atomic write, one "write" WAL entry, catalog, file.write NOTIFY). The
// splice runs on the server's faithful bytes under the lock; see spliceAppend for
// position/anchor semantics. It does NOT create a new file: an append to an absent
// path returns ErrFileNotFound and writes nothing (requireExisting=true) — creating
// a file is write_file's job, where the format allowlist is enforced. (B-36;
// 2026-06-22 no-create fix.)
func (s *FSGitStorage) AppendToFile(ctx context.Context, sessionID, namespace, projectName, path, content, position, anchor string, ifMatch *string) (string, error) {
	return s.writeTransformed(ctx, sessionID, namespace, projectName, path, ifMatch, true,
		func(current []byte) ([]byte, error) {
			return spliceAppend(current, []byte(content), position, anchor)
		})
}

// PatchFile replaces the single unique occurrence of oldString with newString and
// returns the new etag, reusing the write_file path verbatim via writeTransformed.
// The replace runs on the server's faithful bytes under the lock; see splicePatch
// for the exactly-once rule and typed errors. (B-36.)
func (s *FSGitStorage) PatchFile(ctx context.Context, sessionID, namespace, projectName, path, oldString, newString string, ifMatch *string) (string, error) {
	return s.writeTransformed(ctx, sessionID, namespace, projectName, path, ifMatch, false,
		func(current []byte) ([]byte, error) {
			return splicePatch(current, oldString, newString)
		})
}

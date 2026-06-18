// Package opmeta defines the machine-readable operation metadata Shoka embeds in
// each commit message as a single `Shoka-Op:` trailer line, and the FORMAT gate
// that validates it (the 2026-06-18 deleted-log accuracy directive).
//
// Why a commit-message trailer: Shoka is an agent-facing store and git is a
// non-exposed backend, so a commit message is a fine carrier for structured
// metadata. Both the live deleted-log hook (commitEntry) and the bounded repair
// walk read the SAME truth — this trailer — so they classify a delete-vs-move
// identically and cannot drift. The trailer is appended alongside the existing
// Shoka-User/Shoka-Agent/Shoka-Worker identity trailers; nothing parsed the
// commit message before this, so the new line collides with no reader.
//
// AUTHENTICITY is two gates. This package is only the FORMAT gate: a trailer must
// be valid JSON of the schema (Valid) or it is ignored. The MEANING gate — the
// claim must match the commit's real tree diff — lives in package storage, where
// the diff is available; git's real diff is the final authority, and this
// metadata only DISAMBIGUATES delete-vs-move, and only when it agrees with the
// diff. This package reads no git (pure, like internal/storage/index).
package opmeta

import (
	"encoding/json"
	"strings"
)

// TrailerPrefix is the line prefix carrying the JSON metadata in a commit message.
const TrailerPrefix = "Shoka-Op: "

// Op values. These mirror the WAL entry's Op (internal/storage/wal). A move
// carries From (the source path); write/delete do not.
const (
	OpWrite  = "write"
	OpDelete = "delete"
	OpMove   = "move"
)

// Meta is the operation metadata for one commit. Path is the affected
// within-project path (for a move it is the DESTINATION); From is the source path
// and is present ONLY for a move. There is deliberately no timestamp — the
// commit's own author/committer time is authoritative and already in git.
//
// It is a struct so fields can be added later with no migration: an older trailer
// decodes with new fields zero-valued.
type Meta struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	From string `json:"from,omitempty"`
}

// Valid is the FORMAT gate: op must be one of the three known values, path must
// be non-empty, From must be present iff op is "move". A Meta that fails this is
// treated as absent (the caller falls back to raw-diff classification).
func (m Meta) Valid() bool {
	if m.Path == "" {
		return false
	}
	switch m.Op {
	case OpWrite, OpDelete:
		return m.From == ""
	case OpMove:
		return m.From != ""
	default:
		return false
	}
}

// Trailer renders m as the single commit-message trailer line (prefix + compact
// JSON + a trailing newline), to be appended after the identity trailers. It
// assumes m is valid; the caller builds it from the WAL entry.
func Trailer(m Meta) string {
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return TrailerPrefix + string(b) + "\n"
}

// Parse extracts the operation metadata from a commit message, applying the
// format gate. It returns ok=false when no `Shoka-Op:` line is present, the JSON
// is malformed, or the decoded Meta fails Valid — every "ignore it" case the
// caller treats as absent metadata. The last well-formed line wins (commit
// messages are single-writer here, so there is normally exactly one).
func Parse(message string) (Meta, bool) {
	var (
		out Meta
		ok  bool
	)
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, TrailerPrefix) {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, TrailerPrefix))
		var m Meta
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if !m.Valid() {
			continue
		}
		out, ok = m, true
	}
	return out, ok
}

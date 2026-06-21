package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// History read messages (B-31 phase 2). All three are READ-ONLY: they delegate
// to the existing lock-free storage reads (GetHistory / ReadFileAtVersion / the
// phase-1 DiffVersions), introduce no lock, no new git surface outside
// internal/storage (Anchor 1), and write no ref (Anchors 2/3). The History view
// is per-file: a file's commit list → a chosen version's content → a diff of two
// versions.
const (
	// MsgGetHistory requests the commit list for one file; the response carries,
	// per commit, the subject + commit date + committer + hash ONLY. There is NO
	// changed-file list: Shoka commits one file per commit, so a file list would
	// be meaningless (every commit is the one file).
	MsgGetHistory MessageType = "GET_HISTORY"
	// MsgGetFileAt returns a file's content at one explicit commit.
	MsgGetFileAt MessageType = "GET_FILE_AT"
	// MsgGetDiff returns the structured diff of one file between two explicit
	// commits (the phase-1 storage.FileDiff, serialised verbatim incl. Suppressed/
	// Binary so the UI can show the suppression banner instead of an empty diff).
	MsgGetDiff MessageType = "GET_DIFF"
)

// defaultHistoryLimit bounds a GET_HISTORY response when the caller passes no
// (or a non-positive) limit, so a deep history does not stream unbounded.
const defaultHistoryLimit = 100

// GetHistoryPayload is the GET_HISTORY request body.
type GetHistoryPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	Limit       int    `json:"limit,omitempty"`
}

// HistoryCommit is one row of the GET_HISTORY response: the operator-settled
// summary set (subject + commit date + committer) plus the hash. No file list.
type HistoryCommit struct {
	Hash       string    `json:"hash"`
	Subject    string    `json:"subject"`
	Committer  string    `json:"committer"`
	CommitDate time.Time `json:"commitDate"`
}

// HistoryPayload is the GET_HISTORY response body.
type HistoryPayload struct {
	Commits []HistoryCommit `json:"commits"`
}

// GetFileAtPayload is the GET_FILE_AT request body.
type GetFileAtPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	Hash        string `json:"hash"`
}

// FileAtPayload is the GET_FILE_AT response body: a version's content.
type FileAtPayload struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Content string `json:"content"`
}

// GetDiffPayload is the GET_DIFF request body.
type GetDiffPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	FromHash    string `json:"fromHash"`
	ToHash      string `json:"toHash"`
}

// handleGetHistory returns the commit list for one file via the lock-free
// storage.GetHistory, mapping each commit to its subject (the first line of the
// message) + commit date + committer + hash. It surfaces no changed-file list.
func (m *Manager) handleGetHistory(client *uiws.Client, payload json.RawMessage) {
	var p GetHistoryPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for GET_HISTORY")
		return
	}

	limit := p.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}

	commits, err := m.storage.GetHistory(p.Namespace, p.ProjectName, p.Path, limit)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to get history: %v", err))
		return
	}

	rows := make([]HistoryCommit, 0, len(commits))
	for _, c := range commits {
		rows = append(rows, HistoryCommit{
			Hash:       c.Hash,
			Subject:    subjectOf(c.Message),
			Committer:  c.Committer,
			CommitDate: c.CommitDate,
		})
	}
	client.SendResponse(MsgGetHistory, HistoryPayload{Commits: rows})
}

// handleGetFileAt returns a file's content at one explicit commit via the
// lock-free storage.ReadFileAtVersion.
func (m *Manager) handleGetFileAt(client *uiws.Client, payload json.RawMessage) {
	var p GetFileAtPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for GET_FILE_AT")
		return
	}

	content, err := m.storage.ReadFileAtVersion(p.Namespace, p.ProjectName, p.Path, p.Hash)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to read file version: %v", err))
		return
	}

	client.SendResponse(MsgGetFileAt, FileAtPayload{Path: p.Path, Hash: p.Hash, Content: content})
}

// handleGetDiff returns the structured diff of one file between two explicit
// commits via the phase-1 lock-free storage.DiffVersions, serialised verbatim.
// The read loop has no per-request context; DiffVersions applies its own
// deadline internally (context.WithTimeout), so the phase-1 time-cap still holds.
func (m *Manager) handleGetDiff(client *uiws.Client, payload json.RawMessage) {
	var p GetDiffPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for GET_DIFF")
		return
	}

	diff, err := m.storage.DiffVersions(context.Background(), p.Namespace, p.ProjectName, p.Path, p.FromHash, p.ToHash)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to diff versions: %v", err))
		return
	}

	client.SendResponse(MsgGetDiff, diff)
}

// subjectOf returns the first line of a commit message (the subject), trimmed.
// Shoka commit messages are "subject\n\n<Shoka-* trailers>"; the UI shows only
// the subject.
func subjectOf(message string) string {
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		return strings.TrimSpace(message[:i])
	}
	return strings.TrimSpace(message)
}

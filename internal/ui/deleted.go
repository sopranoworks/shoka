package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The /ws/ui deleted-file log handlers (B-28, the 2026-06-18 deleted-log
// directive). They wire storage.ListDeleted (cheap O(cap) read) and
// storage.ReviveFile (forward-only re-create). Both are admin-only, gated at the
// dispatch authzGate via wsLevels — this is the authoritative gate; hiding the UI
// is not sufficient.

// ListDeletedRequest is the LIST_DELETED payload. The namespace/projectName keys
// match wsTarget so the authz gate scopes the admin check to the target namespace.
type ListDeletedRequest struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

// DeletedEntry is one currently-deleted file in the LIST_DELETED response.
type DeletedEntry struct {
	Path           string    `json:"path"`
	DeletionCommit string    `json:"deletionCommit"`
	DeletedAt      time.Time `json:"deletedAt"`
}

// ListDeletedResponse carries the deleted set back (same message type).
type ListDeletedResponse struct {
	Namespace   string         `json:"namespace"`
	ProjectName string         `json:"projectName"`
	Deleted     []DeletedEntry `json:"deleted"`
}

// ReviveFileRequest is the REVIVE_FILE payload.
type ReviveFileRequest struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	FromCommit  string `json:"fromCommit,omitempty"`
}

// ReviveAckPayload is the REVIVE_ACK response.
type ReviveAckPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	Revived     bool   `json:"revived"`
}

func (m *Manager) handleListDeleted(client *wsClient, payload json.RawMessage) {
	var p ListDeletedRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for LIST_DELETED")
		return
	}
	recs, err := m.storage.ListDeleted(p.Namespace, p.ProjectName)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list deleted files: %v", err))
		return
	}
	entries := make([]DeletedEntry, 0, len(recs))
	for _, r := range recs {
		entries = append(entries, DeletedEntry{
			Path:           r.Path,
			DeletionCommit: r.DeletionCommit,
			DeletedAt:      r.DeletedAt,
		})
	}
	client.sendResponse(MsgListDeleted, ListDeletedResponse{
		Namespace:   p.Namespace,
		ProjectName: p.ProjectName,
		Deleted:     entries,
	})
}

func (m *Manager) handleReviveFile(client *wsClient, payload json.RawMessage) {
	var p ReviveFileRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for REVIVE_FILE")
		return
	}
	if p.Path == "" {
		client.sendError("path is required for REVIVE_FILE")
		return
	}
	if err := m.storage.ReviveFile(context.Background(), p.Namespace, p.ProjectName, p.Path, p.FromCommit); err != nil {
		// The typed divergence error (and any other failure) is surfaced clearly —
		// never a silent failure. The client shows it as a non-fatal error toast.
		client.sendError(fmt.Sprintf("Failed to revive file: %v", err))
		return
	}
	client.sendResponse(MsgReviveAck, ReviveAckPayload{
		Namespace:   p.Namespace,
		ProjectName: p.ProjectName,
		Path:        p.Path,
		Revived:     true,
	})
}

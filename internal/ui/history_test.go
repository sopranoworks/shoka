package ui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/storage"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// TestManager_HistoryReads drives the three B-31 phase-2 read messages
// (GET_HISTORY / GET_FILE_AT / GET_DIFF) end-to-end over a real /ws/ui
// connection against a seeded project with a known two-version file history. It
// asserts: GET_HISTORY returns the commit list carrying subject + commit date +
// committer (and NO changed-file list field); GET_FILE_AT returns a version's
// content; GET_DIFF returns the structured FileDiff.
func TestManager_HistoryReads(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shoka-ui-history-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer s.Close()
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Two versions of one file, drained to git so the commits exist.
	if err := s.WriteFile("ns", "proj", "doc.md", "line1\nline2\n"); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain after v1")
	}
	if err := s.WriteFile("ns", "proj", "doc.md", "line1\nCHANGED\n"); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain after v2")
	}

	dm, err := drafts.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("drafts: %v", err)
	}
	m := NewManager(s, dm, nil)
	server := httptest.NewServer(m)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()

	// --- GET_HISTORY ---
	hist := historyRoundTrip(t, conn, MsgGetHistory,
		`{"namespace":"ns","projectName":"proj","path":"doc.md"}`)

	// Decode commits as raw maps first to assert NO changed-file list field, then
	// as the typed payload.
	var rawHist struct {
		Commits []map[string]json.RawMessage `json:"commits"`
	}
	if err := json.Unmarshal(hist, &rawHist); err != nil {
		t.Fatalf("decode raw history: %v", err)
	}
	if len(rawHist.Commits) != 2 {
		t.Fatalf("expected 2 commits, got %d (%s)", len(rawHist.Commits), string(hist))
	}
	for i, c := range rawHist.Commits {
		for _, banned := range []string{"files", "changedFiles", "changed_files", "files_changed", "stat", "shortstat"} {
			if _, ok := c[banned]; ok {
				t.Fatalf("commit %d carries a changed-file/stat field %q (single-file commits ⇒ none allowed): %v", i, banned, c)
			}
		}
		for _, want := range []string{"hash", "subject", "committer", "commitDate"} {
			if _, ok := c[want]; !ok {
				t.Fatalf("commit %d missing field %q: %v", i, want, c)
			}
		}
	}

	var histPayload HistoryPayload
	if err := json.Unmarshal(hist, &histPayload); err != nil {
		t.Fatalf("decode history payload: %v", err)
	}
	newest := histPayload.Commits[0] // GetHistory is newest-first
	oldest := histPayload.Commits[1]
	if newest.Subject == "" || newest.Committer == "" {
		t.Fatalf("commit summary missing subject/committer: %+v", newest)
	}
	if newest.CommitDate.IsZero() {
		t.Fatalf("commit date is zero: %+v", newest)
	}
	if !strings.Contains(newest.Subject, "doc.md") {
		t.Fatalf("subject %q should reference the file (single-file commit subject)", newest.Subject)
	}
	t.Logf("committer surfaced as %q (the owning-user identity); subject=%q", newest.Committer, newest.Subject)

	// --- GET_FILE_AT (the oldest version) ---
	at := historyRoundTrip(t, conn, MsgGetFileAt,
		`{"namespace":"ns","projectName":"proj","path":"doc.md","hash":"`+oldest.Hash+`"}`)
	var fileAt FileAtPayload
	if err := json.Unmarshal(at, &fileAt); err != nil {
		t.Fatalf("decode file-at: %v", err)
	}
	if fileAt.Content != "line1\nline2\n" {
		t.Fatalf("GET_FILE_AT content = %q, want the v1 content", fileAt.Content)
	}

	// --- GET_DIFF (oldest → newest) ---
	diffRaw := historyRoundTrip(t, conn, MsgGetDiff,
		`{"namespace":"ns","projectName":"proj","path":"doc.md","fromHash":"`+oldest.Hash+`","toHash":"`+newest.Hash+`"}`)
	var diff storage.FileDiff
	if err := json.Unmarshal(diffRaw, &diff); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if diff.Status != "modified" {
		t.Fatalf("diff status = %q, want modified", diff.Status)
	}
	if diff.Suppressed != "" {
		t.Fatalf("diff unexpectedly suppressed: %q", diff.Suppressed)
	}
	if len(diff.Hunks) == 0 {
		t.Fatalf("diff has no hunks: %+v", diff)
	}
	// The structured hunk must carry the changed lines (line2 -> CHANGED).
	var sawDelete, sawAdd bool
	for _, h := range diff.Hunks {
		for _, l := range h.Lines {
			if l.Op == "delete" && l.Text == "line2" {
				sawDelete = true
			}
			if l.Op == "add" && l.Text == "CHANGED" {
				sawAdd = true
			}
		}
	}
	if !sawDelete || !sawAdd {
		t.Fatalf("diff hunks missing the expected change (delete line2 / add CHANGED): %+v", diff.Hunks)
	}
}

// roundTrip sends one request frame and returns the next non-NOTIFY response
// payload, failing on an ERROR frame.
func historyRoundTrip(t *testing.T, conn *websocket.Conn, typ MessageType, payloadJSON string) json.RawMessage {
	t.Helper()
	req := uiws.WSMessage{Type: typ, Payload: json.RawMessage(payloadJSON)}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		var resp uiws.WSMessage
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("read %s response: %v", typ, err)
		}
		if resp.Type == MsgNotify {
			continue
		}
		if resp.Type == uiws.Error {
			t.Fatalf("%s returned ERROR: %s", typ, string(resp.Payload))
		}
		return resp.Payload
	}
}

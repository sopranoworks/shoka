package ui

import (
	"encoding/json"
	"testing"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// The SEARCH_FILES request wires the existing project-scoped storage.SearchFiles
// to /ws/ui (session-4 search). These tests exercise the request/response cycle
// over a real ws connection using the shared versioningFixture (project ns/proj
// with one committed file "f.md" containing "v1").

func decodeSearch(t *testing.T, resp uiws.WSMessage) SearchResultPayload {
	t.Helper()
	if resp.Type != MsgSearchResult {
		t.Fatalf("type = %s, want SEARCH_RESULT (payload=%s)", resp.Type, resp.Payload)
	}
	var out SearchResultPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal SEARCH_RESULT: %v", err)
	}
	return out
}

func TestWSUI_SearchByContent(t *testing.T) {
	conn, _, _, _ := versioningFixture(t)

	resp := roundTrip(t, conn, MsgSearchFiles,
		`{"namespace":"ns","projectName":"proj","query":"v1","search_in":"content"}`)
	out := decodeSearch(t, resp)

	if len(out.Matches) != 1 {
		t.Fatalf("matches = %d, want 1 (%+v)", len(out.Matches), out.Matches)
	}
	if out.Matches[0].Path != "f.md" {
		t.Fatalf("match path = %q, want f.md", out.Matches[0].Path)
	}
}

func TestWSUI_SearchByFilename(t *testing.T) {
	conn, _, _, _ := versioningFixture(t)

	resp := roundTrip(t, conn, MsgSearchFiles,
		`{"namespace":"ns","projectName":"proj","query":"f.md","search_in":"filename"}`)
	out := decodeSearch(t, resp)

	if len(out.Matches) != 1 || out.Matches[0].Path != "f.md" {
		t.Fatalf("filename search = %+v, want one match f.md", out.Matches)
	}
}

func TestWSUI_SearchNoResultsReturnsEmptySlice(t *testing.T) {
	conn, _, _, _ := versioningFixture(t)

	resp := roundTrip(t, conn, MsgSearchFiles,
		`{"namespace":"ns","projectName":"proj","query":"no-such-content-anywhere"}`)
	out := decodeSearch(t, resp)

	if len(out.Matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(out.Matches))
	}
	// The wire shape must be [] not null, so a client can iterate unconditionally.
	if !json.Valid(resp.Payload) || string(mustField(t, resp.Payload, "matches")) != "[]" {
		t.Fatalf("matches field = %s, want []", mustField(t, resp.Payload, "matches"))
	}
}

func TestWSUI_SearchDefaultsToBoth(t *testing.T) {
	conn, _, _, _ := versioningFixture(t)

	// No search_in: storage defaults to "both", so a filename substring matches.
	resp := roundTrip(t, conn, MsgSearchFiles,
		`{"namespace":"ns","projectName":"proj","query":"f.md"}`)
	out := decodeSearch(t, resp)

	if len(out.Matches) != 1 || out.Matches[0].Path != "f.md" {
		t.Fatalf("default search = %+v, want one match f.md", out.Matches)
	}
}

func mustField(t *testing.T, payload json.RawMessage, field string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m[field]
}

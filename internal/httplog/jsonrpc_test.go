package httplog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactedRequest_WriteFileContentRedacted(t *testing.T) {
	const content = "SECRET-FILE-BODY-do-not-log-123"
	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"write_file","arguments":{"namespace":"ns","project_name":"proj","path":"a.md","content":"` + content + `","expected_version":"abc123"}}}`)

	method, id, params, ok := redactedRequest(body)
	if !ok {
		t.Fatal("expected ok=true for a well-formed request")
	}
	if method != "tools/call" {
		t.Errorf("method = %q, want tools/call", method)
	}
	if id != "7" {
		t.Errorf("id = %q, want 7", id)
	}
	if strings.Contains(params, content) {
		t.Fatalf("raw content leaked into params: %s", params)
	}
	if want := redactedPlaceholder(len(content)); !strings.Contains(params, want) {
		t.Errorf("params missing placeholder %q: %s", want, params)
	}
	for _, s := range []string{`"project_name":"proj"`, `"path":"a.md"`, `"expected_version":"abc123"`, `"namespace":"ns"`} {
		if !strings.Contains(params, s) {
			t.Errorf("params missing verbatim %s: %s", s, params)
		}
	}
}

func TestRedactedRequest_ReadFileVerbatim(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"q1","method":"tools/call","params":{"name":"read_file","arguments":{"project_name":"proj","path":"a.md"}}}`)
	method, id, params, ok := redactedRequest(body)
	if !ok || method != "tools/call" {
		t.Fatalf("ok=%v method=%q", ok, method)
	}
	if id != `"q1"` {
		t.Errorf("id = %q, want \"q1\" (string id kept as JSON)", id)
	}
	if strings.Contains(params, "<redacted") {
		t.Errorf("read_file params must not be redacted: %s", params)
	}
	if !strings.Contains(params, `"path":"a.md"`) {
		t.Errorf("params missing path: %s", params)
	}
}

func TestRedactedRequest_InitializeHasNoToolRedaction(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
	method, _, params, ok := redactedRequest(body)
	if !ok || method != "initialize" {
		t.Fatalf("ok=%v method=%q", ok, method)
	}
	if !strings.Contains(params, "protocolVersion") {
		t.Errorf("initialize params should be verbatim: %s", params)
	}
}

func TestRedactedRequest_Malformed(t *testing.T) {
	if _, _, _, ok := redactedRequest([]byte(`{not json`)); ok {
		t.Error("expected ok=false for malformed JSON")
	}
	if _, _, _, ok := redactedRequest([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); ok {
		t.Error("expected ok=false for a response (no method)")
	}
}

func TestRedactedResponse_ReadFileContentRedactedBothPlaces(t *testing.T) {
	const body = "FULL-FILE-BODY-THAT-IS-SECRET-xyz"
	mirror, _ := json.Marshal(map[string]any{"content": body, "version": "v1hash"})
	resp := map[string]any{
		"jsonrpc": "2.0", "id": 9,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"content": body, "version": "v1hash"},
		},
	}
	data, _ := json.Marshal(resp)

	id, redacted, ok := redactedResponse(data)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if id != "9" {
		t.Errorf("id = %q, want 9", id)
	}
	if strings.Contains(redacted, body) {
		t.Fatalf("file body leaked: %s", redacted)
	}
	if !strings.Contains(redacted, "v1hash") {
		t.Errorf("version must remain verbatim: %s", redacted)
	}
	if c := strings.Count(redacted, redactedPlaceholder(len(body))); c < 2 {
		t.Errorf("body must be redacted in BOTH structuredContent and the text mirror (got %d): %s", c, redacted)
	}
}

func TestRedactedResponse_ReadSummaryExcerptRedacted(t *testing.T) {
	const excerpt = "leading paragraph of the document body"
	mirror, _ := json.Marshal(map[string]any{"heading": "Title", "excerpt": excerpt, "size": 4096, "version": "v2"})
	resp := map[string]any{
		"jsonrpc": "2.0", "id": 3,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"heading": "Title", "excerpt": excerpt, "size": 4096, "version": "v2"},
		},
	}
	data, _ := json.Marshal(resp)

	_, redacted, ok := redactedResponse(data)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if strings.Contains(redacted, excerpt) {
		t.Fatalf("excerpt leaked: %s", redacted)
	}
	for _, verbatim := range []string{`"heading":"Title"`, "v2", `"size":4096`} {
		if !strings.Contains(redacted, verbatim) {
			t.Errorf("metadata must remain verbatim (%s): %s", verbatim, redacted)
		}
	}
}

func TestRedactedResponse_NestedContentNotRedacted(t *testing.T) {
	const snippet = "matched snippet text"
	sc := map[string]any{"matches": []any{map[string]any{"path": "a.md", "content": snippet}}}
	mirror, _ := json.Marshal(sc)
	resp := map[string]any{
		"jsonrpc": "2.0", "id": 5,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": sc,
		},
	}
	data, _ := json.Marshal(resp)

	_, redacted, ok := redactedResponse(data)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(redacted, snippet) {
		t.Errorf("nested snippet must remain verbatim: %s", redacted)
	}
	if strings.Contains(redacted, "<redacted") {
		t.Errorf("nothing should be redacted in a search result: %s", redacted)
	}
}

func TestRedactedResponse_ErrorEnvelopeVerbatim(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"method \"tools/call\" is invalid during session initialization"}}`)
	id, redacted, ok := redactedResponse(data)
	if !ok {
		t.Fatal("error envelope should parse")
	}
	if id != "3" {
		t.Errorf("id=%q", id)
	}
	if !strings.Contains(redacted, "invalid during session initialization") {
		t.Errorf("error message must be verbatim: %s", redacted)
	}
}

func TestRedactedResponse_ErrorResultTextVerbatim(t *testing.T) {
	resp := map[string]any{
		"jsonrpc": "2.0", "id": 8,
		"result": map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text", "text": "version conflict: file is now at deadbeef"}},
		},
	}
	data, _ := json.Marshal(resp)
	_, redacted, ok := redactedResponse(data)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(redacted, "version conflict: file is now at deadbeef") {
		t.Errorf("error-result text must be verbatim: %s", redacted)
	}
}

func TestRedactedResponse_ReadFileAtVersionContentRedacted(t *testing.T) {
	// read_file_at_version returns {content}. Named explicitly per §4.
	const body = "VERSIONED-FILE-BODY-secret-7g8h"
	mirror, _ := json.Marshal(map[string]any{"content": body})
	resp := map[string]any{
		"jsonrpc": "2.0", "id": 11,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"content": body},
		},
	}
	data, _ := json.Marshal(resp)
	_, redacted, ok := redactedResponse(data)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if strings.Contains(redacted, body) {
		t.Fatalf("file body leaked: %s", redacted)
	}
	if c := strings.Count(redacted, redactedPlaceholder(len(body))); c < 2 {
		t.Errorf("body must be redacted in both places (got %d): %s", c, redacted)
	}
}

func TestRedactedRequest_WriteFileWithoutContentArg(t *testing.T) {
	// A write_file call missing the content argument must not panic and must pass
	// the remaining args through verbatim.
	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_file","arguments":{"project_name":"proj","path":"a.md"}}}`)
	_, _, params, ok := redactedRequest(body)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if strings.Contains(params, "<redacted") {
		t.Errorf("no content arg means no redaction: %s", params)
	}
	if !strings.Contains(params, `"path":"a.md"`) {
		t.Errorf("args must be verbatim: %s", params)
	}
}

// redactResult must NEVER echo raw bytes when the result is valid JSON but not an
// object (the no-leak invariant outranks completeness). A bare JSON string that
// looked like a file body must be replaced with the content-safe marker.
func TestRedactResult_NonObjectResultNotEchoedRaw(t *testing.T) {
	const fakeBody = "PRETEND-FILE-BODY-as-a-bare-json-string-5z"
	raw, _ := json.Marshal(fakeBody) // a valid JSON string, not an object
	out := redactResult(raw)
	if strings.Contains(string(out), fakeBody) {
		t.Fatalf("non-object result must not be echoed raw: %s", out)
	}
	if !strings.Contains(string(out), "unredactable result") {
		t.Errorf("expected content-safe marker, got: %s", out)
	}
}

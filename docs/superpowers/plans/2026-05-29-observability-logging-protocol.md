# Protocol-Level Logging (Observability Stage 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add DEBUG-level, observation-only visibility of MCP/JSON-RPC wire traffic (request bodies, response bodies, full SSE event payloads) so the next dogfooding run can be diagnosed by reading logs alone — without changing any wire behavior.

**Architecture:** All new visibility is added at the **transport layer** (`internal/httplog`), which is the only point that sees the failing scenario completely. The decisive SDK fact: a `tools/call` arriving before `initialize` is rejected in `ServerSession.handle` (`server.go:1437`) *before* any receiving middleware runs — so middleware-based logging would miss the failing call's params. The transport layer sees that POST body, and the SSE `message` frames there *are* the JSON-RPC responses (so §3.2 ⊂ §3.3), and bodies there carry the `rpc_id` middleware cannot reach. Session-state (§3.4) is already emitted by the SDK through the stage-1-wired `ServerOptions.Logger`; no code is added for it. Everything new is gated behind `logger.Enabled(ctx, DEBUG)` so INFO-level production overhead is identical to stage 1.

**Tech Stack:** Go 1.26, stdlib `log/slog` + `encoding/json`, mcp-go-sdk v1.6.0 (SSE/HTTP). No new dependencies.

**Hard constraints (from the directive — every task must honor these):**
- **Observation only.** No change to MCP session handling, SSE control flow, tool/storage/auth/webhook behavior, or the wire contract. The bytes on the wire must be **unchanged**. The recorder writes the original bytes to the client *first and unmodified*, then logs a redacted **copy**.
- **Logging is best-effort.** A panic in any logging path is recovered and never reaches the client or aborts a request.
- **Redaction (§4) — redact ONLY these, log everything else verbatim:**
  - `write_file` `arguments.content` (request) → `<redacted N bytes>` (N = byte length).
  - `read_file` / `read_file_at_version` result `content`, and `read_summary` result `excerpt` → `<redacted N bytes>`. The SDK mirrors structured output into BOTH `result.structuredContent` AND `result.content[].text` (a JSON string), so redaction must hit both.
  - Never log: `Authorization` header value, `?token=` query value, `X-Shoka-Signature`. (These never reach the bodies/frames we log; the design simply never reads request headers.)
  - The `?sessionid=` value is **NOT** a secret and **must** be logged (it is the correlation key the directive explicitly wants).
- The existing `tests/logging_secret_test.go::TestLogging_NeverLeaksContentOrToken` must keep passing **unchanged**.
- **No diagnosis** in the completion report.

**Key implementation facts (verified against SDK v1.6.0 source):**
- SSE responses go out on the **GET stream's** `http.ResponseWriter` as `message` events (`sse.go:332` → `event.go:44`); the POST returns 202 with no body. Each event is one `w.Write` of `event: NAME\ndata: PAYLOAD\n\n` (`event.go:44-60`). Flush uses `http.NewResponseController(w).Flush()`, so a wrapper must preserve `Unwrap()`.
- The `endpoint` event (`sse.go:182`) carries `?sessionid=...` — the real session id, which `Session.ID()` does NOT expose for SSE (`sse.go:297` hardcodes `""`).
- Successful tool handlers return `(nil, typedOut, nil)`; the SDK sets `StructuredContent = json(out)` (`server.go:384`) AND, because `Content == nil`, also `Content = [TextContent{Text: json(out)}]` (`server.go:386-393`). Hence the double-mirror.
- Only the three read tools have a top-level `content`/`excerpt` field in their output structs (verified across all 13 tools). So redacting *top-level* `content`/`excerpt` keys targets exactly the read tools and nothing else — no id→method correlation needed.

---

## File Structure

- **Create** `internal/httplog/jsonrpc.go` — pure JSON-RPC parse + §4 redaction helpers.
- **Create** `internal/httplog/jsonrpc_test.go` — unit tests for redaction.
- **Create** `internal/httplog/sse.go` — SSE response-stream teeing recorder + frame parser.
- **Create** `internal/httplog/sse_test.go` — unit tests for the recorder (incl. bytes-unchanged invariant).
- **Modify** `internal/httplog/httplog.go` — wire DEBUG request-body logging + SSE recorder into `Middleware`; remove the now-superseded `peekMethod`/`methodRe`.
- **Create** `tests/logging_protocol_test.go` — end-to-end DEBUG protocol logging over a real authenticated SSE session.
- **Modify** `docs/OPERATIONS.md` — document the new DEBUG protocol output.
- **Modify** `docs/contracts/mcp-v1.md` — one non-normative operational note (no normative change).
- **Modify/Create** `.planning/deferred.md` — record SDK-limitation observations (§6).
- **Create** `meta/reports/2026-05-29-shoka-logging-protocol-complete.md` — completion report with real sample lines.

---

### Task 1: Redaction core (`internal/httplog/jsonrpc.go`)

**Files:**
- Create: `internal/httplog/jsonrpc.go`
- Test: `internal/httplog/jsonrpc_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/httplog/jsonrpc_test.go`:

```go
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
		t.Fatal("ok")
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
	// search_files-shaped result: a NESTED "content" field (a snippet) must stay
	// verbatim — only TOP-LEVEL content/excerpt is a read-tool body. This proves
	// the redaction does not over-reach (directive §4: everything else verbatim).
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
		t.Fatal("ok")
	}
	if !strings.Contains(redacted, snippet) {
		t.Errorf("nested snippet must remain verbatim: %s", redacted)
	}
	if strings.Contains(redacted, "<redacted") {
		t.Errorf("nothing should be redacted in a search result: %s", redacted)
	}
}

func TestRedactedResponse_ErrorEnvelopeVerbatim(t *testing.T) {
	// THE failing-scenario response: a JSON-RPC error. It carries no file content
	// and must be logged verbatim (error messages are verbatim per §4).
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
	// A tool-level failure (IsError result): content[0].text is an error STRING,
	// not JSON, so it must pass through verbatim (not mistaken for a read body).
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
		t.Fatal("ok")
	}
	if !strings.Contains(redacted, "version conflict: file is now at deadbeef") {
		t.Errorf("error-result text must be verbatim: %s", redacted)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail (compile error)**

Run: `cd internal/httplog && go test ./... -run TestRedacted -v`
Expected: FAIL — `undefined: redactedRequest`, `undefined: redactedResponse`, `undefined: redactedPlaceholder`.

- [ ] **Step 3: Implement `internal/httplog/jsonrpc.go`**

```go
// jsonrpc.go adds DEBUG protocol-level observation helpers to httplog: parsing
// JSON-RPC request/response envelopes off the wire and applying the directive's
// §4 redaction rules before anything is logged. These helpers are pure and never
// touch the bytes that flow to the client or the handler — callers always log a
// redacted COPY. The secret-non-logging invariant outranks completeness: on any
// parse failure we emit a content-safe placeholder rather than raw bytes.
package httplog

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// redactedPlaceholder is the §4 replacement for a withheld string: the value is
// dropped, but its byte length is preserved as a diagnostic signal.
func redactedPlaceholder(byteLen int) string {
	return fmt.Sprintf("<redacted %d bytes>", byteLen)
}

// rpcRequest / rpcResponse are the minimal JSON-RPC shapes we log. Params/Result/
// Error stay raw so only the parts we redact are re-marshalled.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// redactedRequest parses a JSON-RPC request body and returns its method, id (as
// compact JSON; empty for notifications), and params re-serialized as JSON with
// §4 redaction applied. ok is false if the body is not a JSON-RPC request we can
// parse (the caller then logs a content-safe "unparseable" line).
func redactedRequest(body []byte) (method, id, params string, ok bool) {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", "", false
	}
	if req.Method == "" {
		return "", "", "", false
	}
	return req.Method, compactID(req.ID), redactParams(req.Params), true
}

// redactedResponse parses a JSON-RPC response (the data payload of an SSE
// "message" event) and returns its id and the whole envelope re-serialized with
// §4 read-content redaction applied. ok is false if data is not a parseable
// JSON-RPC response (result or error present).
func redactedResponse(data []byte) (id, redacted string, ok bool) {
	var resp rpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", false
	}
	if len(resp.Result) == 0 && len(resp.Error) == 0 {
		return "", "", false
	}
	resp.Result = redactResult(resp.Result)
	out, err := marshalNoEscape(resp)
	if err != nil {
		return "", "", false
	}
	return compactID(resp.ID), string(out), true
}

// redactParams redacts the write_file content argument (§4). Only tools/call
// params for write_file carry a content argument, so this is exactly targeted;
// every other field stays verbatim.
func redactParams(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	m, err := decodeObject(raw)
	if err != nil {
		return fmt.Sprintf("<unparseable params %d bytes>", len(raw))
	}
	if name, _ := m["name"].(string); name == "write_file" {
		if args, ok := m["arguments"].(map[string]any); ok {
			if c, ok := args["content"].(string); ok {
				args["content"] = redactedPlaceholder(len(c))
			}
		}
	}
	out, err := marshalNoEscape(m)
	if err != nil {
		return fmt.Sprintf("<unmarshalable params %d bytes>", len(raw))
	}
	return string(out)
}

// redactResult redacts read-tool file content (§4) from a tools/call result. The
// SDK mirrors structured output into BOTH result.structuredContent AND
// result.content[].text (a JSON string), so we redact the TOP-LEVEL content/
// excerpt keys in both places. Only read_file/read_file_at_version (content) and
// read_summary (excerpt) expose those top-level keys, so this targets exactly the
// read tools; error-message text (not JSON) and search/list nested fields stay
// verbatim. Anything unparseable is returned unchanged (errors carry no body).
func redactResult(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	m, err := decodeObject(raw)
	if err != nil {
		return raw
	}
	if sc, ok := m["structuredContent"].(map[string]any); ok {
		redactBodyFields(sc)
	}
	if items, ok := m["content"].([]any); ok {
		for _, it := range items {
			cm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := cm["text"].(string); ok {
				if red, changed := redactEmbeddedJSON(txt); changed {
					cm["text"] = red
				}
			}
		}
	}
	out, err := marshalNoEscape(m)
	if err != nil {
		return raw
	}
	return out
}

// redactBodyFields redacts the top-level file-body fields of a read result in place.
func redactBodyFields(obj map[string]any) {
	for _, key := range []string{"content", "excerpt"} {
		if v, ok := obj[key].(string); ok {
			obj[key] = redactedPlaceholder(len(v))
		}
	}
}

// redactEmbeddedJSON redacts top-level content/excerpt fields inside a JSON object
// encoded as a string (the SDK's text mirror of structured output). It returns
// (input, false) when the text is not a JSON object or has no such field — which
// leaves error-message strings and non-read results untouched.
func redactEmbeddedJSON(text string) (string, bool) {
	m, err := decodeObject(json.RawMessage(text))
	if err != nil {
		return text, false
	}
	changed := false
	for _, key := range []string{"content", "excerpt"} {
		if v, ok := m[key].(string); ok {
			m[key] = redactedPlaceholder(len(v))
			changed = true
		}
	}
	if !changed {
		return text, false
	}
	out, err := marshalNoEscape(m)
	if err != nil {
		return text, false
	}
	return string(out), true
}

// decodeObject unmarshals a JSON object into a map, preserving numbers exactly so
// re-marshalling does not corrupt ids/sizes/etc.
func decodeObject(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// compactID renders a JSON-RPC id (number or string) as compact JSON; "" if absent.
func compactID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return ""
	}
	return buf.String()
}

// marshalNoEscape marshals compactly without HTML-escaping <, >, & so logged
// JSON stays readable. (We are logging a redacted view, not preserving wire bytes.)
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd internal/httplog && go test ./... -run TestRedacted -v`
Expected: PASS (all sub-tests).

- [ ] **Step 5: Commit**

```bash
git add internal/httplog/jsonrpc.go internal/httplog/jsonrpc_test.go
git commit -m "feat(httplog): JSON-RPC parse + §4 redaction helpers for protocol logging"
```

---

### Task 2: SSE response-stream teeing recorder (`internal/httplog/sse.go`)

**Files:**
- Create: `internal/httplog/sse.go`
- Test: `internal/httplog/sse_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/httplog/sse_test.go`:

```go
package httplog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestSSERecorder builds an sseLogRecorder over an httptest recorder, logging
// at DEBUG into sink.
func newTestSSERecorder(under http.ResponseWriter, sink io.Writer) *sseLogRecorder {
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sr := &statusRecorder{ResponseWriter: under, status: http.StatusOK}
	return &sseLogRecorder{statusRecorder: sr, logger: logger, ctx: context.Background(), connID: "c1"}
}

func TestSSELogRecorder_ForwardsBytesUnchanged(t *testing.T) {
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, io.Discard)
	frame := []byte("event: endpoint\ndata: ?sessionid=ABC\n\n")
	n, err := w.Write(frame)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(frame) {
		t.Errorf("n = %d, want %d", n, len(frame))
	}
	if under.Body.String() != string(frame) {
		t.Errorf("client bytes altered: %q", under.Body.String())
	}
}

func TestSSELogRecorder_EndpointEventLoggedVerbatim(t *testing.T) {
	var sink bytes.Buffer
	w := newTestSSERecorder(httptest.NewRecorder(), &sink)
	w.Write([]byte("event: endpoint\ndata: ?sessionid=SESS123\n\n"))
	logs := sink.String()
	for _, want := range []string{"sse event sent", "endpoint", "SESS123", "conn_id=c1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

func TestSSELogRecorder_MessageReadResultRedacted(t *testing.T) {
	const body = "SECRET-BODY-on-the-wire-987"
	mirror, _ := json.Marshal(map[string]any{"content": body, "version": "vhash"})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 4,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"content": body, "version": "vhash"},
		},
	})
	frame := append([]byte("event: message\ndata: "), resp...)
	frame = append(frame, '\n', '\n')

	var sink bytes.Buffer
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, &sink)
	w.Write(frame)

	if under.Body.String() != string(frame) {
		t.Fatal("client bytes must be unchanged")
	}
	logs := sink.String()
	if strings.Contains(logs, body) {
		t.Fatalf("file body leaked into logs: %s", logs)
	}
	for _, want := range []string{"mcp response sent", "rpc_id=4", "vhash"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

func TestSSELogRecorder_MultipleFramesInOneWrite(t *testing.T) {
	var sink bytes.Buffer
	w := newTestSSERecorder(httptest.NewRecorder(), &sink)
	w.Write([]byte("event: endpoint\ndata: ?sessionid=A\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
	logs := sink.String()
	if strings.Count(logs, "event_name=endpoint") != 1 {
		t.Errorf("expected one endpoint log: %s", logs)
	}
	if !strings.Contains(logs, "rpc_id=1") {
		t.Errorf("expected message response logged: %s", logs)
	}
}

func TestSSELogRecorder_FrameSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := newTestSSERecorder(httptest.NewRecorder(), &sink)
	w.Write([]byte("event: endpoint\ndata: ?sess"))
	if strings.Contains(sink.String(), "sse event sent") {
		t.Fatal("must not log a partial frame")
	}
	w.Write([]byte("ionid=Z\n\n"))
	if !strings.Contains(sink.String(), "sessionid=Z") {
		t.Errorf("frame completed across writes should log: %s", sink.String())
	}
}

func TestSSELogRecorder_PreservesFlusherViaResponseController(t *testing.T) {
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, io.Discard)
	// The SDK flushes via http.NewResponseController(w).Flush(); it must find the
	// flusher by unwrapping our recorder. This must not error.
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("Flush via ResponseController failed: %v", err)
	}
}

func TestParseSSEFrame(t *testing.T) {
	ev, data := parseSSEFrame([]byte("event: message\ndata: {\"a\":1}"))
	if ev != "message" || data != `{"a":1}` {
		t.Errorf("got (%q,%q)", ev, data)
	}
	ev, data = parseSSEFrame([]byte("data: line1\ndata: line2"))
	if ev != "" || data != "line1\nline2" {
		t.Errorf("multi-line data: got (%q,%q)", ev, data)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd internal/httplog && go test ./... -run 'TestSSELogRecorder|TestParseSSEFrame' -v`
Expected: FAIL — `undefined: sseLogRecorder`, `undefined: parseSSEFrame`.

- [ ] **Step 3: Implement `internal/httplog/sse.go`**

```go
// sse.go provides DEBUG-level observation of the outbound SSE stream. It wraps the
// http.ResponseWriter the SDK writes events to, forwards every byte UNCHANGED to
// the client, and logs a redacted COPY of each complete SSE event frame: the
// endpoint event (which carries ?sessionid=...) and message events (JSON-RPC
// responses, with §4 read-content redaction). It never alters wire bytes,
// ordering, or flushing; logging is best-effort and a logging panic can never
// reach the client.
package httplog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
)

// sseLogRecorder tees the SSE response stream for logging. It embeds
// *statusRecorder so status capture, Flush, and Unwrap (for
// http.ResponseController) are preserved unchanged.
type sseLogRecorder struct {
	*statusRecorder
	logger *slog.Logger
	ctx    context.Context
	connID string
	acc    []byte // accumulates bytes until a complete "\n\n"-delimited frame
}

// Write forwards the original bytes to the client first and unchanged, then
// observes a copy for logging.
func (w *sseLogRecorder) Write(b []byte) (int, error) {
	n, err := w.statusRecorder.Write(b)
	w.observe(b)
	return n, err
}

// observe appends written bytes and logs each complete SSE frame. Best-effort:
// any panic in the logging path is swallowed so it can never affect the response.
func (w *sseLogRecorder) observe(b []byte) {
	defer func() { _ = recover() }()
	w.acc = append(w.acc, b...)
	for {
		idx := bytes.Index(w.acc, []byte("\n\n"))
		if idx < 0 {
			break
		}
		w.logFrame(w.acc[:idx])
		w.acc = w.acc[idx+2:]
	}
	if len(w.acc) == 0 {
		w.acc = nil
	}
}

func (w *sseLogRecorder) logFrame(frame []byte) {
	event, data := parseSSEFrame(frame)
	if event == "" && data == "" {
		return // keepalive comment / empty frame
	}
	if event == "message" {
		if id, redacted, ok := redactedResponse([]byte(data)); ok {
			w.logger.LogAttrs(w.ctx, slog.LevelDebug, "mcp response sent",
				slog.String("conn_id", w.connID),
				slog.String("event_name", event),
				slog.String("rpc_id", id),
				slog.String("event_data", redacted))
			return
		}
		// Unparseable message payload: log its size, never its raw bytes.
		w.logger.LogAttrs(w.ctx, slog.LevelDebug, "sse event sent",
			slog.String("conn_id", w.connID),
			slog.String("event_name", event),
			slog.Int("data_bytes", len(data)))
		return
	}
	// endpoint, ping, etc.: framing data carries no file content or credential.
	w.logger.LogAttrs(w.ctx, slog.LevelDebug, "sse event sent",
		slog.String("conn_id", w.connID),
		slog.String("event_name", event),
		slog.String("event_data", data))
}

// parseSSEFrame extracts the event name and concatenated data payload from one
// SSE frame (the bytes before a "\n\n" delimiter).
func parseSSEFrame(frame []byte) (event, data string) {
	for _, line := range strings.Split(string(frame), "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(line[len("data:"):], " ")
			if data == "" {
				data = d
			} else {
				data += "\n" + d
			}
		}
	}
	return event, data
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd internal/httplog && go test ./... -run 'TestSSELogRecorder|TestParseSSEFrame' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httplog/sse.go internal/httplog/sse_test.go
git commit -m "feat(httplog): SSE stream teeing recorder (forwards bytes unchanged, logs redacted frames)"
```

---

### Task 3: Wire DEBUG observation into `Middleware`

**Files:**
- Modify: `internal/httplog/httplog.go`

**Context:** The current `Middleware` peeks 1 KiB of the POST body for the method (`peekMethod`/`methodRe`) and logs INFO stream open/close + DEBUG "mcp message received" + WARN on ≥400. This task: (a) gate a full-body parse (Task 1) behind DEBUG and enrich "mcp message received" with `rpc_id`+`rpc_params`; (b) wrap the GET stream's ResponseWriter with the Task-2 recorder when DEBUG; (c) keep INFO/WARN behavior identical; (d) remove the now-superseded `peekMethod` and `methodRe`. INFO-level behavior must be byte-for-byte the same as stage 1 (no body read at non-DEBUG).

- [ ] **Step 1: Replace the body of `internal/httplog/httplog.go`**

Replace the entire file with:

```go
// Package httplog provides transport-layer logging for Shoka's MCP (SSE) endpoint.
// At INFO it logs SSE stream lifecycle and rejected requests (metadata only,
// never headers — so Authorization/?token= are never logged). At DEBUG it adds
// protocol-level observation: full JSON-RPC request/response bodies and SSE event
// payloads, with the directive's §4 redaction applied (see jsonrpc.go, sse.go).
// All DEBUG work is gated on logger.Enabled so INFO-level overhead is unchanged.
package httplog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Middleware logs SSE GET stream open/close (INFO), POST JSON-RPC messages
// (DEBUG, redacted), outbound SSE events (DEBUG, redacted), and any response with
// status >= 400 (WARN). A nil logger is replaced with a discard logger.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sessionID := r.URL.Query().Get("sessionid")
			debug := logger.Enabled(r.Context(), slog.LevelDebug)

			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			var rw http.ResponseWriter = sr

			switch r.Method {
			case http.MethodGet:
				connID := newConnID()
				logger.LogAttrs(r.Context(), slog.LevelInfo, "sse stream opened",
					slog.String("conn_id", connID), slog.String("remote", r.RemoteAddr))
				if debug {
					rw = &sseLogRecorder{statusRecorder: sr, logger: logger, ctx: r.Context(), connID: connID}
				}
				next.ServeHTTP(rw, r)
				logger.LogAttrs(r.Context(), slog.LevelInfo, "sse stream closed",
					slog.String("conn_id", connID), slog.String("remote", r.RemoteAddr),
					slog.Int("status", sr.status), slog.Duration("duration", time.Since(start)))
			case http.MethodPost:
				if debug {
					logRequest(r, logger, sessionID)
				}
				next.ServeHTTP(rw, r)
			default:
				next.ServeHTTP(rw, r)
			}

			if sr.status >= 400 {
				logger.LogAttrs(r.Context(), slog.LevelWarn, "request rejected",
					slog.String("http_method", r.Method), slog.String("path", r.URL.Path),
					slog.String("session_id", sessionID), slog.Int("status", sr.status),
					slog.String("remote", r.RemoteAddr))
			}
		})
	}
}

// logRequest reads the full POST body, restores it byte-identically for the
// downstream handler, and logs the JSON-RPC method/id/params at DEBUG with §4
// redaction. Best-effort: a panic here never reaches the handler, and the body is
// always restored even on a read error. Only called when DEBUG is enabled, so the
// full-body read never happens at production INFO level.
func logRequest(r *http.Request, logger *slog.Logger, sessionID string) {
	defer func() { _ = recover() }()
	if r.Body == nil {
		return
	}
	body, _ := io.ReadAll(r.Body)
	// Restore the exact bytes (plus any unread remainder) for the handler.
	r.Body = &restoredBody{Reader: io.MultiReader(bytes.NewReader(body), r.Body), closer: r.Body}

	method, id, params, ok := redactedRequest(body)
	if !ok {
		// Content-safe: never log raw bytes we could not structurally redact.
		logger.LogAttrs(r.Context(), slog.LevelDebug, "mcp message received (unparseable)",
			slog.String("session_id", sessionID),
			slog.Int("body_bytes", len(body)),
			slog.String("remote", r.RemoteAddr))
		return
	}
	attrs := []slog.Attr{
		slog.String("rpc_method", method),
		slog.String("session_id", sessionID),
		slog.String("remote", r.RemoteAddr),
	}
	if id != "" {
		attrs = append(attrs, slog.String("rpc_id", id))
	}
	if params != "" {
		attrs = append(attrs, slog.String("rpc_params", params))
	}
	logger.LogAttrs(r.Context(), slog.LevelDebug, "mcp message received", attrs...)
}

type restoredBody struct {
	io.Reader
	closer io.Closer
}

func (b *restoredBody) Close() error { return b.closer.Close() }

func newConnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the response status while forwarding everything to the
// underlying ResponseWriter. It preserves http.Flusher (directly and via Unwrap,
// for http.ResponseController) so SSE streaming is unaffected.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
```

- [ ] **Step 2: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: clean (no unused `regexp`, no undefined symbols).

- [ ] **Step 3: Run the full httplog + existing suites**

Run: `go test ./internal/httplog/... ./tests/... -race`
Expected: PASS — including the pre-existing `tests/logging_secret_test.go` and `tests/wire_schema_test.go`, proving no observable behavior change.

- [ ] **Step 4: Commit**

```bash
git add internal/httplog/httplog.go
git commit -m "feat(httplog): wire DEBUG protocol logging into Middleware (gated; INFO unchanged)"
```

---

### Task 4: End-to-end protocol logging test

**Files:**
- Create: `tests/logging_protocol_test.go`

**Context:** `tests/logging_secret_test.go` (same package) already defines `syncBuffer`, `bearerRT`, and `wireText`. Reuse them. This test drives a real authenticated SSE session at DEBUG through create_project → write_file (secret content) → read_file, then asserts the four §3 layers are visible and §4 redaction holds. When `SHOKA_DUMP_LOGS=1`, it dumps the captured log via `t.Log` so real sample lines can be lifted for the completion report (Task 6).

- [ ] **Step 1: Write the test**

Create `tests/logging_protocol_test.go`:

```go
package tests

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/httplog"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/require"
)

// TestLogging_Protocol drives a real authenticated SSE session at DEBUG and
// asserts the four protocol layers (§3.1 request, §3.2 response, §3.3 SSE events,
// §3.4 SDK session state) are visible, with §4 redaction applied to write_file
// content and read_file result content. The secret invariant (content + token
// never logged) is also re-checked here.
func TestLogging_Protocol(t *testing.T) {
	const (
		token   = "PROTO-SECRET-BEARER-7e8f9a"
		content = "PROTOCOL-TEST-FILE-BODY-never-log-44d1c2"
	)

	sink := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := storage.NewFSGitStorage(t.TempDir())
	require.NoError(t, err)
	s.SetLogger(logger)

	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-proto-test", Version: "0.0.0"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srv, &mcp.Tool{Name: "create_project"}, tools.LoggedTool(logger, "create_project", tools.CreateProjectHandler(s)))
	mcp.AddTool(srv, &mcp.Tool{Name: "write_file"}, tools.LoggedTool(logger, "write_file", tools.WriteFileHandler(s)))
	mcp.AddTool(srv, &mcp.Tool{Name: "read_file"}, tools.LoggedTool(logger, "read_file", tools.ReadFileHandler(s)))

	// Production-shaped chain: logging outermost, then auth, then SSE handler.
	sseHandler := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{token}})
	handler := httplog.Middleware(logger)(a.Middleware(sseHandler))

	httpSrv := httptest.NewServer(handler)
	defer httpSrv.Close()

	client := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}}
	cli := mcp.NewClient(&mcp.Implementation{Name: "proto-test-client", Version: "0.0.0"}, nil)
	sess, err := cli.Connect(context.Background(), &mcp.SSEClientTransport{Endpoint: httpSrv.URL, HTTPClient: client}, nil)
	require.NoError(t, err)
	defer sess.Close()

	r, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_project", Arguments: map[string]any{"project_name": "p"}})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	r, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "write_file", Arguments: map[string]any{"project_name": "p", "path": "a.md", "content": content}})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	r, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"project_name": "p", "path": "a.md"}})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	logs := sink.String()
	if os.Getenv("SHOKA_DUMP_LOGS") != "" {
		t.Log("\n--- BEGIN CAPTURED DEBUG LOG ---\n" + logs + "\n--- END CAPTURED DEBUG LOG ---")
	}

	// Invariants (must hold under the new logging — §4 / §7.2).
	require.NotContains(t, logs, content, "file content must never appear in logs")
	require.NotContains(t, logs, token, "bearer token must never appear in logs")

	// §3.1 request visibility + write_file content redaction.
	require.Contains(t, logs, "mcp message received", "§3.1 request line")
	require.Contains(t, logs, "rpc_method=tools/call")
	require.Contains(t, logs, "rpc_id=")
	require.Contains(t, logs, "<redacted ", "write_file content must be redacted with a byte-length placeholder")
	require.Contains(t, logs, "a.md", "non-secret path stays verbatim in params")

	// §3.2 + §3.3 response/SSE event visibility.
	require.Contains(t, logs, "mcp response sent", "§3.2 response line")
	require.Contains(t, logs, "event_name=endpoint", "§3.3 endpoint event must be logged")
	require.Contains(t, logs, "sessionid=", "endpoint payload (session id) must be visible")

	// §3.4 session state (emitted by the SDK via the wired logger).
	require.Contains(t, logs, "server session connected")
	require.Contains(t, logs, "session initialized")
}
```

- [ ] **Step 2: Run the new test (race)**

Run: `go test ./tests/ -run TestLogging_Protocol -race -v`
Expected: PASS. (If a response/SSE assertion is flaky due to goroutine timing, that is a real ordering bug — investigate; do NOT add sleeps without understanding why.)

- [ ] **Step 3: Confirm the invariant test still passes UNCHANGED**

Run: `go test ./tests/ -run TestLogging_NeverLeaksContentOrToken -race -v`
Expected: PASS, with `tests/logging_secret_test.go` unmodified.

- [ ] **Step 4: Commit**

```bash
git add tests/logging_protocol_test.go
git commit -m "test(logging): end-to-end DEBUG protocol logging + §4 redaction over real SSE"
```

---

### Task 5: Docs + deferred observations

**Files:**
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/contracts/mcp-v1.md`
- Modify/Create: `.planning/deferred.md`

**Context:** Read the current Logging section of `docs/OPERATIONS.md` and the "Operational notes" appendix of `docs/contracts/mcp-v1.md` (added in stage 1) before editing, and match their style. The contract must stay byte-accurate on all NORMATIVE text — only a non-normative operational note may be added (§7.5).

- [ ] **Step 1: Extend `docs/OPERATIONS.md`**

In the existing Logging section, add a subsection documenting DEBUG protocol output. Use this content (adapt heading depth to the surrounding document):

```markdown
#### Protocol-level output at `debug`

At `server.log.level: debug` the MCP endpoint additionally emits redacted,
protocol-level traces to stderr to make wire-level faults diagnosable:

- `mcp message received` — each inbound JSON-RPC request: `rpc_method`, `rpc_id`,
  and `rpc_params` (the full params as JSON). The `write_file` `content` argument
  is replaced with `<redacted N bytes>`; everything else is verbatim.
- `mcp response sent` — each outbound JSON-RPC response carried on the SSE stream:
  `rpc_id` and the full response `event_data`. `read_file` /
  `read_file_at_version` `content` and `read_summary` `excerpt` are replaced with
  `<redacted N bytes>`; everything else (including version hashes and error
  messages) is verbatim.
- `sse event sent` — every other SSE event, including the `endpoint` event whose
  `event_data` carries the `?sessionid=...` correlation value.
- Session lifecycle (`server session connected`, `session initialized`,
  `server session disconnected`, and the `method invalid during initialization`
  rejection) is emitted by the MCP SDK itself via the configured logger.

This output is best-effort diagnostic instrumentation only; it never changes the
wire protocol, and file contents and bearer tokens are never logged. It is
verbose — enable `debug` for diagnosis, not for steady-state operation.
```

- [ ] **Step 2: Add a non-normative note to `docs/contracts/mcp-v1.md`**

Append one bullet to the existing non-normative "Operational notes" appendix (do NOT alter any normative section):

```markdown
- **Diagnostic logging (non-normative).** When the server is run with
  `server.log.level: debug`, it emits redacted protocol-level traces (JSON-RPC
  request/response bodies and SSE event payloads) to stderr. This is operational
  instrumentation only: it does not change the wire protocol, message shapes, or
  any behavior described above, and never logs file contents or credentials.
```

- [ ] **Step 3: Record SDK-limitation observations in `.planning/deferred.md`**

Append (create the file if absent) an "Observability stage 2 — observations" section. **These are observations only — no diagnosis, no hypotheses.**

```markdown
## Observability stage 2 (protocol logging) — observations / deferred

Recorded per the protocol-logging directive §6 (limitations, not diagnoses):

- The MCP SDK's SSE transport hardcodes `Session.ID()` to `""`
  (`sse.go:297` in go-sdk v1.6.0), so the SDK's own session-state log lines carry
  an empty `session_id`. The real session id is observed instead from the
  `endpoint` SSE event payload and the `?sessionid=` query value at the transport
  layer. Not worked around in the SDK.
- Session-state notifications (§3.4) are emitted by the SDK through the configured
  `ServerOptions.Logger` at the SDK's own levels (INFO for connected / initialized
  / disconnected; ERROR for "method invalid during initialization"), not at DEBUG.
  They were not relabelled.
- A `tools/call` arriving before `initialize` is rejected by the SDK before any
  receiving middleware runs, so request params for such calls are observable only
  at the transport layer (where this implementation logs them), not via SDK
  middleware. JSON-RPC ids are likewise only reachable at the transport layer.
- Deferred (out of scope here): typed structured-logging of tool errors; UI
  WebSocket (`/ws/ui`, `/drafts/`) protocol logging; metrics/tracing; log
  rotation/shipping.
```

Do not `git add -f` if `.planning/` is ignored — let normal `git add` behavior apply; the on-disk note is what matters.

- [ ] **Step 4: Build docs sanity + commit**

Run: `go build ./...` (sanity; docs-only change).
Then:
```bash
git add docs/OPERATIONS.md docs/contracts/mcp-v1.md
git add .planning/deferred.md 2>/dev/null || true
git commit -m "docs: document DEBUG protocol logging (OPERATIONS + non-normative contract note) + deferred observations"
```

---

### Task 6: Completion report with real sample lines

**Files:**
- Create: `meta/reports/2026-05-29-shoka-logging-protocol-complete.md`

**Context:** Read `docs/conventions/frontmatter.md` and the stage-1 report `meta/reports/2026-05-28-shoka-logging-complete.md` to mirror the frontmatter and structure. The report must contain **no diagnosis, interpretation, or hypothesis** about the session-init fault (directive §2, §7) — it describes only what was added and what the logs look like.

- [ ] **Step 1: Capture real sample log lines**

Run the integration test with the dump flag and capture output:

```bash
SHOKA_DUMP_LOGS=1 go test ./tests/ -run TestLogging_Protocol -v 2>&1 | sed -n '/BEGIN CAPTURED DEBUG LOG/,/END CAPTURED DEBUG LOG/p'
```

From the captured block, copy one real line for each of: `mcp message received` (§3.1, ideally the write_file call showing `<redacted N bytes>`), `mcp response sent` (§3.2, ideally the read_file response showing redaction + verbatim version), `sse event sent` with `event_name=endpoint` (§3.3), and a session-state line such as `session initialized` (§3.4).

- [ ] **Step 2: Write `meta/reports/2026-05-29-shoka-logging-protocol-complete.md`**

Frontmatter per the convention (mirror the stage-1 report), then sections:

1. **Summary** — one paragraph: protocol-level DEBUG observation added at the transport layer; observation-only; no diagnosis.
2. **What was added (§3.1–§3.4)** — the four layers, each with **one real sample log line** captured in Step 1.
3. **Redaction (§4)** — the rules implemented and the tests proving them: `internal/httplog/jsonrpc_test.go` (write_file content, read_file content in both mirror sites, read_summary excerpt, nested-content NOT redacted, error verbatim), `internal/httplog/sse_test.go` (bytes-unchanged + redaction over a real frame), and the end-to-end `tests/logging_protocol_test.go`.
4. **Invariant** — confirm `TestLogging_NeverLeaksContentOrToken` passes unchanged.
5. **SDK limitations encountered (observations only)** — the four bullets from `.planning/deferred.md` Task 5 Step 3 (empty SSE `Session.ID()`; SDK session-state at its own levels; pre-init rejection bypasses middleware; ids only at transport). State them as observations; **do not** explain what they imply about the fault.
6. **Verification** — `go build ./...`, `go vet ./...`, `go test ./... -race` all pass on host; note container run (Task 7 / finish step).
7. **Commits** — list the commits introduced by this plan.

Close with the operator handoff (verbatim intent from directive §8): the operator runs with `server.log.level: debug`, reproduces the failing scenario, and the captured log feeds the *next* directive. **No diagnosis here.**

- [ ] **Step 3: Commit**

```bash
git add meta/reports/2026-05-29-shoka-logging-protocol-complete.md
git commit -m "docs(report): protocol-logging (observability stage 2) completion report"
```

---

## Final verification (after all tasks)

- [ ] `go build ./...` — clean
- [ ] `go vet ./...` — clean
- [ ] `go test ./... -race` — all pass on host (darwin, go1.26.2)
- [ ] Container parity: `docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm go test ./... -race` (devcontainer-equivalent; matches stage-1 verification practice)
- [ ] `git diff master -- docs/contracts/mcp-v1.md` shows only the non-normative appendix bullet (no normative change)
- [ ] `tests/logging_secret_test.go` is unmodified (`git diff master -- tests/logging_secret_test.go` is empty)

## Self-review notes (spec coverage)

- §3.1 request method/id/params (redacted) → Task 3 `logRequest` + Task 1 `redactedRequest`. ✓
- §3.2 response id/result/error (redacted) → Task 2 `logFrame` "message" branch + Task 1 `redactedResponse`. ✓
- §3.3 SSE event_name/event_data/conn_id incl. `endpoint` → Task 2 `sseLogRecorder`. ✓
- §3.4 SDK session state → already wired (stage 1); confirmed by Task 4 assertions; documented as observation. ✓
- §4 redaction (write_file content; read content/excerpt in both mirror sites; everything else verbatim; never headers/token) → Task 1 + tests; `?sessionid=` deliberately NOT redacted. ✓
- §5 stderr, text+json formats accommodate new fields (slog attrs work in both), DEBUG only → Task 3 gating. ✓
- §7.2 invariant test unchanged → Task 4 Step 3. ✓
- §7.5 contract byte-accurate (non-normative note only) → Task 5 + final verification. ✓
- §6/§7 SDK limitations recorded; no diagnosis → Task 5 + Task 6. ✓
```
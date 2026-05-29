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
// verbatim. Anything unparseable is replaced with a content-safe marker — never
// returned raw — because the no-leak invariant (§4) outranks completeness.
func redactResult(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	m, err := decodeObject(raw)
	if err != nil {
		// Result is valid JSON (the envelope parsed) but not an object; never
		// echo it raw — defensively assume it could carry a body.
		return unredactableMarker(len(raw))
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
		return unredactableMarker(len(raw))
	}
	return out
}

// unredactableMarker is the content-safe JSON value substituted when a result
// cannot be parsed or re-marshalled for redaction. It echoes only the byte
// length, never the raw bytes (which could contain file content).
func unredactableMarker(byteLen int) json.RawMessage {
	// marshalNoEscape of a string never fails, so the error is unreachable here.
	out, _ := marshalNoEscape(fmt.Sprintf("<unredactable result %d bytes>", byteLen))
	return json.RawMessage(out)
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

// compactID renders a JSON-RPC id (number, string, or null) as compact JSON; ""
// if the field is absent.
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

package reqtrace

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B-56: with the dump switch OFF, no dump line is emitted and the existing B-53
// lines + the response are unchanged.
func TestDump_Off_NoDumpLines_BehaviourUnchanged(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "web", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello")))

	out := buf.String()
	if strings.Contains(out, "http request dump") || strings.Contains(out, "http response dump") {
		t.Fatalf("dump emitted while switch OFF:\n%s", out)
	}
	if !strings.Contains(out, "request received") || !strings.Contains(out, "request completed") {
		t.Errorf("existing B-53 lines missing while OFF:\n%s", out)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("response altered while OFF: %q", rec.Body.String())
	}
}

// B-56 §5: with the switch ON, for a request on EACH surface, the verbatim request
// (method/path/all headers/full body) and verbatim response (status/all headers/full
// body) are emitted, correlated by ONE request_id, with no field selection.
func TestDump_On_CompleteCapture_AllSurfaces_OneID(t *testing.T) {
	for _, surface := range []string{"web", "mcp-plain", "mcp-oauth"} {
		t.Run(surface, func(t *testing.T) {
			logger, buf := jsonLogger()
			h := Middleware(logger, surface, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				w.Header().Set("X-Custom", "verbatim-value")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"echo":"` + string(b) + `"}`))
			}))
			req := httptest.NewRequest(http.MethodPost, "/path?foo=bar", strings.NewReader("REQBODY"))
			req.Header.Set("X-Test", "t-val")
			h.ServeHTTP(httptest.NewRecorder(), req)

			out := buf.String()
			// Verbatim request: method, full path+query, the custom header, full body.
			for _, want := range []string{"http request dump", `"http_method":"POST"`, `/path?foo=bar`, "X-Test: t-val", "REQBODY"} {
				if !strings.Contains(out, want) {
					t.Errorf("request dump missing %q on %s:\n%s", want, surface, out)
				}
			}
			// Verbatim response: status, the custom header, full body.
			for _, want := range []string{"http response dump", `"status":201`, "X-Custom: verbatim-value", `echo`, "REQBODY"} {
				if !strings.Contains(out, want) {
					t.Errorf("response dump missing %q on %s:\n%s", want, surface, out)
				}
			}
			// One shared request_id across received + request dump + response dump + completed.
			ids := extractIDs(out)
			if len(ids) != 4 {
				t.Fatalf("expected 4 request_id-bearing lines on %s, got %d:\n%s", surface, len(ids), out)
			}
			for _, id := range ids {
				if id != ids[0] {
					t.Errorf("request_id not shared across dump lines on %s: %v", surface, ids)
				}
			}
		})
	}
}

// B-59 §5: NO redaction. The /token request (the secret-bearing one whose request dump
// the redaction path was dropping) is dumped VERBATIM — code/code_verifier/consent and
// the Authorization value all appear in clear, and the «redacted» marker NEVER appears.
// This replaces B-56's TestDump_On_RedactionMaskedAndMinimal: the decision is reversed
// by operator direction for this local, default-OFF debug switch.
func TestDump_On_NoRedaction_Verbatim(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "mcp-oauth", true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The MCP/OAuth path consumes the request body first — the dump must STILL have it.
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"RESP_SECRET_TOKEN","token_type":"Bearer"}`))
	}))
	form := "grant_type=authorization_code&code=AUTH_SECRET_CODE&code_verifier=VERIFIER_SECRET&consent_credential=CONSENT_SECRET&client_id=public-client"
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer BEARER_SECRET_TOKEN")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()

	// The /token request dump is present with its full form body in clear.
	if !strings.Contains(out, "http request dump") {
		t.Fatalf("the /token request dump is missing:\n%s", out)
	}
	// Every secret value is dumped VERBATIM — request body secrets, the Authorization
	// header value, and the response access_token.
	for _, secret := range []string{
		"AUTH_SECRET_CODE", "VERIFIER_SECRET", "CONSENT_SECRET",
		"BEARER_SECRET_TOKEN", "RESP_SECRET_TOKEN",
	} {
		if !strings.Contains(out, secret) {
			t.Errorf("expected secret %q dumped VERBATIM (no redaction), got:\n%s", secret, out)
		}
	}
	// The redaction marker must NEVER appear, and the Authorization header is emitted
	// verbatim (not as a fingerprint).
	if strings.Contains(out, "«redacted»") {
		t.Errorf("redaction marker present — the dump must be raw:\n%s", out)
	}
	if !strings.Contains(out, "Authorization: Bearer BEARER_SECRET_TOKEN") {
		t.Errorf("Authorization header not emitted verbatim:\n%s", out)
	}
	if strings.Contains(out, "fingerprint=") {
		t.Errorf("Authorization fingerprint substitution present — the dump must be raw:\n%s", out)
	}
}

// B-56 §5: behaviour-preserving — the handler receives the unmodified request body and
// the client receives the byte-identical response whether the dump is OFF or ON.
func TestDump_BehaviourPreserving_OnVsOff(t *testing.T) {
	run := func(dump bool) (handlerSawBody string, rec *httptest.ResponseRecorder) {
		logger, _ := jsonLogger()
		h := Middleware(logger, "web", dump)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			handlerSawBody = string(b)
			w.Header().Set("X-H", "v")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("RESPONSE-BYTES"))
		}))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("REQ-BYTES")))
		return handlerSawBody, rec
	}
	offBody, offRec := run(false)
	onBody, onRec := run(true)

	if onBody != "REQ-BYTES" || onBody != offBody {
		t.Errorf("handler saw different request body: off=%q on=%q", offBody, onBody)
	}
	if onRec.Code != offRec.Code || onRec.Code != http.StatusAccepted {
		t.Errorf("status differs ON vs OFF: off=%d on=%d", offRec.Code, onRec.Code)
	}
	if onRec.Body.String() != offRec.Body.String() || onRec.Body.String() != "RESPONSE-BYTES" {
		t.Errorf("response body differs ON vs OFF: off=%q on=%q", offRec.Body.String(), onRec.Body.String())
	}
	if onRec.Header().Get("X-H") != "v" {
		t.Errorf("response header altered by dump: %q", onRec.Header().Get("X-H"))
	}
}

// B-59 §5: no-exception pairing. A representative mix — a discovery GET, the /token
// POST (whose body a downstream reader also consumes), an SSE/stream response, an error
// response, and a junk/unparseable POST — driven through one middleware: EVERY
// request_id must have BOTH an `http request dump` and an `http response dump`. None
// missing either half, regardless of method/content/parseability/status.
func TestDump_On_NoExceptionPairing(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "mcp-oauth", true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A downstream reader consumes the body first (the MCP message path does this) —
		// the request dump must still have captured it.
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			_, _ = w.Write([]byte(`{"resource":"x"}`))
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"T"}`))
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte("data: chunk\n\n"))
				fl.Flush()
			}
		case "/boom":
			http.Error(w, "nope", http.StatusInternalServerError)
		default: // junk/unparseable
			_ = body
			http.Error(w, "bad", http.StatusBadRequest)
		}
	}))

	reqs := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil),
		httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=authorization_code&code=C&code_verifier=V")),
		httptest.NewRequest(http.MethodGet, "/sse", nil),
		httptest.NewRequest(http.MethodPost, "/boom", strings.NewReader("{}")),
		httptest.NewRequest(http.MethodPost, "/junk", strings.NewReader("\x00\x01\x02not-json-or-form")),
	}
	for _, req := range reqs {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Tally, per request_id, which dump halves were emitted.
	type halves struct{ req, resp bool }
	seen := map[string]*halves{}
	order := []string{}
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg string `json:"msg"`
			ID  string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("log line not JSON: %q: %v", ln, err)
		}
		if rec.Msg != "http request dump" && rec.Msg != "http response dump" {
			continue
		}
		if seen[rec.ID] == nil {
			seen[rec.ID] = &halves{}
			order = append(order, rec.ID)
		}
		if rec.Msg == "http request dump" {
			seen[rec.ID].req = true
		} else {
			seen[rec.ID].resp = true
		}
	}

	if len(order) != len(reqs) {
		t.Fatalf("expected %d request_ids with dumps, got %d:\n%s", len(reqs), len(order), buf.String())
	}
	for _, id := range order {
		h := seen[id]
		if !h.req || !h.resp {
			t.Errorf("request_id %s missing a dump half (request=%v response=%v) — no exception allowed:\n%s",
				id, h.req, h.resp, buf.String())
		}
	}
}

// B-59 §2.4/§3: the SSE stream is not broken AND its body is now CAPTURED (no longer
// omitted). Flusher still works, the client still receives the streamed bytes, and the
// streamed bytes appear in the response dump — no response is dumped bodyless. This
// replaces B-56's TestDump_SSE_NotBroken_BodyOmitted.
func TestDump_SSE_NotBroken_BodyCaptured(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "mcp-oauth", true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("Flusher lost through the dump recorder — SSE would break")
		}
		_, _ = w.Write([]byte("data: STREAM_CHUNK\n\n"))
		fl.Flush()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(rec.Body.String(), "STREAM_CHUNK") {
		t.Errorf("client did not receive streamed bytes: %q", rec.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, "http response dump") {
		t.Fatalf("the SSE response dump is missing:\n%s", out)
	}
	if !strings.Contains(out, "STREAM_CHUNK") {
		t.Errorf("SSE body was NOT captured into the dump (B-59 captures it):\n%s", out)
	}
	if strings.Contains(out, "body_omitted") {
		t.Errorf("SSE body_omitted marker present — B-59 removed the omission:\n%s", out)
	}
	if !strings.Contains(out, "Content-Type: text/event-stream") || !strings.Contains(out, `"status":200`) {
		t.Errorf("expected SSE headers+status still dumped:\n%s", out)
	}
}

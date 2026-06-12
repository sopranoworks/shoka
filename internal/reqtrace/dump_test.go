package reqtrace

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shoka/mcp-server/internal/tokenfp"
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

// B-56 §5: redaction is correct AND minimal — the fixed secret-list values are masked
// and never appear, while non-secret content is emitted verbatim.
func TestDump_On_RedactionMaskedAndMinimal(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "mcp-oauth", true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Response carries a secret access_token AND a non-secret self-referential issuer.
		_, _ = w.Write([]byte(`{"access_token":"RESP_SECRET_TOKEN","token_type":"Bearer","issuer":"https://ext.example/iss"}`))
	}))
	form := "grant_type=authorization_code&code=AUTH_SECRET_CODE&code_verifier=VERIFIER_SECRET&consent_credential=CONSENT_SECRET&client_id=public-client"
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer BEARER_SECRET_TOKEN")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()

	// No secret value may appear anywhere in the dump.
	for _, secret := range []string{
		"AUTH_SECRET_CODE", "VERIFIER_SECRET", "CONSENT_SECRET",
		"BEARER_SECRET_TOKEN", "RESP_SECRET_TOKEN",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked into dump:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected redaction marker, got:\n%s", out)
	}
	// Authorization value → presence + the B-54 fingerprint (correlates with auth stage).
	fp := tokenfp.Fingerprint("BEARER_SECRET_TOKEN")
	if fp == "" || !strings.Contains(out, "fingerprint="+fp) {
		t.Errorf("expected Authorization fingerprint=%s, got:\n%s", fp, out)
	}
	// Minimal: non-secret content is verbatim — form non-secret fields, the JSON
	// non-secret issuer/endpoints, the status, the content-type header.
	for _, want := range []string{
		"grant_type=authorization_code", // value contains "code" but is NOT a secret key
		"client_id=public-client",
		"ext.example/iss", // the self-referential issuer URL — the whole B-55 point
		"token_type",      // non-secret token-response field, verbatim
		`"status":200`,
		"Content-Type: application/json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected non-secret content %q verbatim, got:\n%s", want, out)
		}
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

// B-56 §3: the SSE stream is not broken — its body is NOT buffered (the documented
// capture limit), Flusher still works, and the client still receives the streamed
// bytes; the dump records headers+status and notes body_omitted=streaming.
func TestDump_SSE_NotBroken_BodyOmitted(t *testing.T) {
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
	if strings.Contains(out, "STREAM_CHUNK") {
		t.Errorf("SSE body was buffered into the dump (should be omitted):\n%s", out)
	}
	if !strings.Contains(out, `"body_omitted":"streaming"`) {
		t.Errorf("expected body_omitted=streaming for the SSE response:\n%s", out)
	}
	if !strings.Contains(out, "Content-Type: text/event-stream") || !strings.Contains(out, `"status":200`) {
		t.Errorf("expected SSE headers+status still dumped:\n%s", out)
	}
}

package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/tokenfp"
)

// B-54 discriminator: the one-way token fingerprint logged at the auth stage lets the
// operator tell, on a freshly-issued token that is rejected as invalid-token, whether
// the SAME value reached Lookup (fingerprint == the "oauth token issued" fingerprint
// → store reset/split, 5a/5b) or a DIFFERENT value arrived (fingerprint differs →
// proxy/stale token, 5c/5d). The token value is never logged.
func TestAuthFingerprint_DiscriminatesSameVsDifferentToken(t *testing.T) {
	issued := "issued-access-token-value-AAAA"
	issuedFP := tokenfp.Fingerprint(issued)

	// (1) MATCH case — the SAME issued value arrives but the store no longer holds it
	// (validator knows nothing; simulates a reset/split store). Reject fingerprint
	// must EQUAL the issuance fingerprint.
	lg, buf := jsonBufLogger()
	emptyStore := New(Config{Logger: lg, ValidateToken: func(string) (Principal, RejectReason, bool) {
		return Principal{}, ReasonInvalidToken, false
	}})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+issued)
	emptyStore.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(httptest.NewRecorder(), req)
	line := findJSONLine(t, buf, "auth rejected")
	if line["reason"] != "invalid-token" || line["token_fingerprint"] != issuedFP {
		t.Errorf("MATCH case: want reason=invalid-token fingerprint=%s, got reason=%v fp=%v", issuedFP, line["reason"], line["token_fingerprint"])
	}
	if strings.Contains(buf.String(), issued) {
		t.Fatalf("token value leaked into the log:\n%s", buf.String())
	}

	// (2) DIFFER case — a DIFFERENT value arrives (the store holds `issued`, but the
	// client/proxy presented something else). Reject fingerprint must DIFFER from the
	// issuance fingerprint and equal the presented value's fingerprint.
	lg2, buf2 := jsonBufLogger()
	realStore := New(Config{Logger: lg2, ValidateToken: func(tok string) (Principal, RejectReason, bool) {
		if tok == issued {
			return Principal{Name: "Op"}, "", true
		}
		return Principal{}, ReasonInvalidToken, false
	}})
	wrong := "a-different-token-BBBB"
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+wrong)
	realStore.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(httptest.NewRecorder(), req2)
	line2 := findJSONLine(t, buf2, "auth rejected")
	if line2["token_fingerprint"] == issuedFP {
		t.Errorf("DIFFER case: fingerprint must differ from issuance, got %v", line2["token_fingerprint"])
	}
	if line2["token_fingerprint"] != tokenfp.Fingerprint(wrong) {
		t.Errorf("DIFFER case: fingerprint must equal the presented value's, got %v", line2["token_fingerprint"])
	}

	// (3) Positive — the issued value validates and "auth ok" carries the same
	// fingerprint, so a SUCCESSFUL validation also correlates to issuance.
	lg3, buf3 := jsonBufLogger()
	realStore3 := New(Config{Logger: lg3, ValidateToken: func(tok string) (Principal, RejectReason, bool) {
		if tok == issued {
			return Principal{Name: "Op"}, "", true
		}
		return Principal{}, ReasonInvalidToken, false
	}})
	req3 := httptest.NewRequest(http.MethodPost, "/", nil)
	req3.Header.Set("Authorization", "Bearer "+issued)
	realStore3.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(httptest.NewRecorder(), req3)
	okLine := findJSONLine(t, buf3, "auth ok")
	if okLine["token_fingerprint"] != issuedFP {
		t.Errorf("auth ok fingerprint: got %v want %s", okLine["token_fingerprint"], issuedFP)
	}
}

// B-53 §2.4 (supersedes B-52's handshake-gated "mcp principal bound"): the OAuth
// auth stage logs its result on EVERY request — "auth ok" (INFO) with the bound
// client/principal + the shared request_id, marking the handshake via is_handshake;
// "auth rejected" (WARN) with a discrete reason. The access token is never logged.

func jsonBufLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})), buf
}

func findJSONLine(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("log line not JSON: %q: %v", ln, err)
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log line with msg=%q in:\n%s", msg, buf.String())
	return nil
}

func TestMiddleware_OAuth_LogsAuthOKOnHandshake(t *testing.T) {
	lg, buf := jsonBufLogger()
	a := New(Config{
		Logger: lg,
		ValidateToken: func(token string) (Principal, RejectReason, bool) {
			if token != "good-token" {
				return Principal{}, ReasonInvalidToken, false
			}
			return Principal{Name: "Operator", Email: "op@example.test", ClientID: "https://connector.example/meta"}, "", true
		},
	})
	served := false
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	// initialize handshake: POST, no Mcp-Session-Id.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !served || rec.Code != http.StatusOK {
		t.Fatalf("handshake not served: served=%v code=%d", served, rec.Code)
	}
	line := findJSONLine(t, buf, "auth ok")
	if line["authenticator"] != "oauth" {
		t.Errorf("authenticator: got %v", line["authenticator"])
	}
	if line["client_id"] != "https://connector.example/meta" {
		t.Errorf("client_id: got %v", line["client_id"])
	}
	if line["principal"] != "Operator" {
		t.Errorf("principal: got %v", line["principal"])
	}
	if line["is_handshake"] != true {
		t.Errorf("is_handshake: want true on the initialize POST, got %v", line["is_handshake"])
	}
	if strings.Contains(buf.String(), "good-token") {
		t.Errorf("access token leaked into the log:\n%s", buf.String())
	}
}

// B-53 §2.4: the auth stage logs ALWAYS, so a subsequent session-bearing request
// also emits "auth ok" — but with is_handshake=false (it is not a new connect).
func TestMiddleware_OAuth_AuthOKOnSubsequentRequestNotHandshake(t *testing.T) {
	lg, buf := jsonBufLogger()
	a := New(Config{
		Logger: lg,
		ValidateToken: func(token string) (Principal, RejectReason, bool) {
			return Principal{Name: "Operator", ClientID: "https://c.example/meta"}, "", true
		},
	})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Mcp-Session-Id", "SID-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	line := findJSONLine(t, buf, "auth ok")
	if line["is_handshake"] != false {
		t.Errorf("is_handshake: want false on a session-bearing request, got %v", line["is_handshake"])
	}
}

// The headline gap: an OAuth rejection must name a discrete reason (not a silent
// 401). missing-bearer vs expired vs invalid-token are all distinguished.
func TestMiddleware_OAuth_RejectionNamesDiscreteReason(t *testing.T) {
	lg, buf := jsonBufLogger()
	a := New(Config{
		Logger: lg,
		ValidateToken: func(token string) (Principal, RejectReason, bool) {
			switch token {
			case "":
				return Principal{}, ReasonMissingBearer, false
			case "expired-token":
				return Principal{}, ReasonExpired, false
			default:
				return Principal{}, ReasonInvalidToken, false
			}
		},
	})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	cases := map[string]string{"": "missing-bearer", "Bearer expired-token": "expired", "Bearer whatever": "invalid-token"}
	for authz, wantReason := range cases {
		buf.Reset()
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("authz=%q: want 401, got %d", authz, rec.Code)
		}
		line := findJSONLine(t, buf, "auth rejected")
		if line["reason"] != wantReason {
			t.Errorf("authz=%q: reason got %v want %q", authz, line["reason"], wantReason)
		}
		if line["authenticator"] != "oauth" {
			t.Errorf("authz=%q: authenticator got %v", authz, line["authenticator"])
		}
	}
}

// The static-bearer path also names its rejection reason (missing vs invalid) — the
// plain-MCP and all Web routes go through here.
func TestMiddleware_Static_RejectionNamesReason(t *testing.T) {
	lg, buf := jsonBufLogger()
	a := New(Config{Logger: lg, Enabled: true, Tokens: []string{"secret"}})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	// Missing bearer.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if line := findJSONLine(t, buf, "auth rejected"); line["reason"] != "missing-bearer" || line["authenticator"] != "static-bearer" {
		t.Errorf("missing: got reason=%v auth=%v", line["reason"], line["authenticator"])
	}
	buf.Reset()
	// Present but wrong.
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if line := findJSONLine(t, buf, "auth rejected"); line["reason"] != "invalid-token" {
		t.Errorf("wrong: got reason=%v", line["reason"])
	}
}

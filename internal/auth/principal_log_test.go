package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B-52 §2.4: on the OAuth-enforced initialize handshake, the bound client/principal
// is logged (so "which client got bound to the session" is visible), never the
// access token, and only once per connect (gated to the handshake request).

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

func TestMiddleware_OAuth_LogsPrincipalBoundOnHandshake(t *testing.T) {
	lg, buf := jsonBufLogger()
	a := New(Config{
		Logger: lg,
		ValidateToken: func(token string) (Principal, bool) {
			if token != "good-token" {
				return Principal{}, false
			}
			return Principal{Name: "Operator", Email: "op@example.test", ClientID: "https://connector.example/meta"}, true
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
	line := findJSONLine(t, buf, "mcp principal bound")
	if line["client_id"] != "https://connector.example/meta" {
		t.Errorf("client_id: got %v", line["client_id"])
	}
	if line["principal"] != "Operator" {
		t.Errorf("principal: got %v", line["principal"])
	}
	if strings.Contains(buf.String(), "good-token") {
		t.Errorf("access token leaked into the log:\n%s", buf.String())
	}
}

// A subsequent (session-bearing) request must NOT re-log the binding — the line is
// gated to the handshake so it fires once per connect, not per request.
func TestMiddleware_OAuth_NoPrincipalLogOnSubsequentRequest(t *testing.T) {
	lg, buf := jsonBufLogger()
	a := New(Config{
		Logger: lg,
		ValidateToken: func(token string) (Principal, bool) {
			return Principal{Name: "Operator", ClientID: "https://c.example/meta"}, true
		},
	})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Mcp-Session-Id", "SID-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(buf.String(), "mcp principal bound") {
		t.Errorf("binding logged on a session-bearing request (should be handshake-only):\n%s", buf.String())
	}
}

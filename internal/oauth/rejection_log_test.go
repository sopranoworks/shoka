package oauth

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
)

// B-51: every OAuth/CIMD client-verification rejection must log (server-side,
// structured) what was received and why, so the empty-list / wrong-domain cases
// are self-diagnosing — without leaking secrets or the trusted-domain values.

// bufLogger returns a JSON slog logger writing into the returned buffer so a test
// can assert the structured attributes of each emitted line.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})), buf
}

// logLineWithMsg finds the single JSON log record whose "msg" equals msg.
func logLineWithMsg(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", ln, err)
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log line with msg=%q in:\n%s", msg, buf.String())
	return nil
}

// loggedAS builds an AuthServer with the given trusted-domain allowlist and a
// buffer-backed logger, without a CIMD network dependency (used by the rejection
// tests, which reject before any fetch).
func loggedAS(t *testing.T, trusted []string) (testAS, *bytes.Buffer) {
	t.Helper()
	store, err := oauthstore.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger, buf := bufLogger()
	as := NewAuthServer(store, NewVerifier(trusted), AuthServerConfig{
		ExternalURL:    "https://rs.example",
		BoundPrincipal: oauthstore.Principal{Name: "Operator", Email: "op@example.test"},
		Logger:         logger,
	})
	return testAS{as: as, store: store}, buf
}

// Empty trusted list (default-deny): the rejection log names the received
// client_id, reason trusted-list-empty, and trusted-count 0 — an operator reading
// it learns exactly what to add. The wire response is unchanged (400 invalid_client).
func TestVerificationLog_EmptyTrustedList(t *testing.T) {
	h, buf := loggedAS(t, nil)
	_, challenge := pkcePair()
	clientID := "https://connector.example/meta"
	rec := h.authorize(t, http.MethodGet, baseAuthForm(clientID, challenge))

	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("wire response changed: want 400 on-page, got %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	line := logLineWithMsg(t, buf, "oauth client verification rejected")
	if line["level"] != "WARN" {
		t.Errorf("rejection must be WARN, got %v", line["level"])
	}
	if line["client_id"] != clientID {
		t.Errorf("client_id: want %q, got %v", clientID, line["client_id"])
	}
	if line["reason"] != "trusted-list-empty" {
		t.Errorf("reason: want trusted-list-empty, got %v", line["reason"])
	}
	if line["trusted_domains_configured"] != float64(0) {
		t.Errorf("trusted_domains_configured: want 0, got %v", line["trusted_domains_configured"])
	}
}

// Configured-but-no-match: with a non-empty allowlist and a non-matching client,
// the log shows the received client_id, the evaluated domain, reason
// domain-not-in-trusted-list, and the count (>0) — and never the trusted VALUE.
func TestVerificationLog_WrongDomain(t *testing.T) {
	// A distinctive trusted value with no substring overlap with the client's
	// domain, so a Contains check can prove the value is not in the log.
	const trustedValue = "allowed-partner.example"
	h, buf := loggedAS(t, []string{trustedValue})
	_, challenge := pkcePair()
	clientID := "https://untrusted.example/meta"
	rec := h.authorize(t, http.MethodGet, baseAuthForm(clientID, challenge))

	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("wire response changed: want 400 on-page, got %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	line := logLineWithMsg(t, buf, "oauth client verification rejected")
	if line["client_id"] != clientID {
		t.Errorf("client_id: want %q, got %v", clientID, line["client_id"])
	}
	if line["evaluated_domain"] != "untrusted.example" {
		t.Errorf("evaluated_domain: want untrusted.example, got %v", line["evaluated_domain"])
	}
	if line["reason"] != "domain-not-in-trusted-list" {
		t.Errorf("reason: want domain-not-in-trusted-list, got %v", line["reason"])
	}
	if line["trusted_domains_configured"] != float64(1) {
		t.Errorf("trusted_domains_configured: want 1, got %v", line["trusted_domains_configured"])
	}
	// The trusted-domain VALUE must never appear in the log (only its count).
	if strings.Contains(buf.String(), trustedValue) {
		t.Errorf("trusted-domain value leaked into the log:\n%s", buf.String())
	}
	// No secret (consent credential) in the log.
	if strings.Contains(buf.String(), testCredential) {
		t.Errorf("consent credential leaked into the log:\n%s", buf.String())
	}
}

// Success path is equally observable: an accepted client logs (info) the accepted
// client_id/domain, and no token/secret appears in the log.
func TestVerificationLog_SuccessAccepted(t *testing.T) {
	cimd := newTLSCIMD(t)
	store, err := oauthstore.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger, buf := bufLogger()
	as := NewAuthServer(store, cimd.verifier, AuthServerConfig{
		ExternalURL:    "https://rs.example",
		BoundPrincipal: oauthstore.Principal{Name: "Operator", Email: "op@example.test"},
		Logger:         logger,
	})
	h := testAS{as: as, store: store}

	_, challenge := pkcePair()
	rec := h.authorize(t, http.MethodGet, baseAuthForm(cimd.clientID, challenge))
	if rec.Code != http.StatusOK {
		t.Fatalf("want consent page 200, got %d", rec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth client verification accepted")
	if line["level"] != "INFO" {
		t.Errorf("success must be INFO, got %v", line["level"])
	}
	if line["client_id"] != cimd.clientID {
		t.Errorf("client_id: want %q, got %v", cimd.clientID, line["client_id"])
	}
	if line["evaluated_domain"] != cimd.host {
		t.Errorf("evaluated_domain: want %q, got %v", cimd.host, line["evaluated_domain"])
	}
	if strings.Contains(buf.String(), testCredential) {
		t.Errorf("consent credential leaked into the log:\n%s", buf.String())
	}
}

// cimdFixture is a working CIMD metadata server plus a verifier trusting it.
type cimdFixture struct {
	verifier *Verifier
	clientID string
	host     string
}

func newTLSCIMD(t *testing.T) cimdFixture {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	t.Cleanup(srv.Close)
	clientID := srv.URL + "/client-meta"
	srv.Config.Handler = docHandler(t, ClientMetadata{
		ClientID:                clientID,
		ClientName:              "Test Client",
		RedirectURIs:            []string{testRedirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethod: "none",
	})
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := NewVerifier([]string{host})
	v.isBlockedIP = func(net.IP) bool { return false }
	v.tlsConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	return cimdFixture{verifier: v, clientID: clientID, host: host}
}

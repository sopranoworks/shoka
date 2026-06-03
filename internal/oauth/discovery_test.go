package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shoka/mcp-server/internal/auth"
)

func getJSON(t *testing.T, h http.Handler, target string) (int, string, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	body := rec.Body.String()
	var m map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("response not valid JSON: %v\n%s", err, body)
		}
	}
	return rec.Code, rec.Header().Get("Content-Type"), m
}

const extURL = "https://public.example"

func TestPRMResourceExactMatchAndASSelf(t *testing.T) {
	h := ProtectedResourceMetadataHandler(DiscoveryConfig{ExternalURL: extURL})
	code, ctype, m := getJSON(t, h, "/.well-known/oauth-protected-resource/mcp")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.HasPrefix(ctype, "application/json") {
		t.Fatalf("expected application/json, got %q", ctype)
	}
	// The #1 discovery-failure cause: resource MUST equal the exact MCP endpoint.
	if m["resource"] != "https://public.example/mcp" {
		t.Fatalf("resource must equal the exact MCP endpoint, got %v", m["resource"])
	}
	servers, _ := m["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != "https://public.example" {
		t.Fatalf("authorization_servers must be [self], got %v", m["authorization_servers"])
	}
}

func TestASMetadataFields(t *testing.T) {
	h := AuthorizationServerMetadataHandler(DiscoveryConfig{ExternalURL: extURL})
	code, _, m := getJSON(t, h, "/.well-known/oauth-authorization-server")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if m["issuer"] != "https://public.example" {
		t.Fatalf("issuer wrong: %v", m["issuer"])
	}
	if m["authorization_endpoint"] != "https://public.example/authorize" {
		t.Fatalf("authorization_endpoint wrong: %v", m["authorization_endpoint"])
	}
	if m["token_endpoint"] != "https://public.example/token" {
		t.Fatalf("token_endpoint wrong: %v", m["token_endpoint"])
	}
	// PKCE S256 mandatory — clients refuse if absent.
	pkce, _ := m["code_challenge_methods_supported"].([]any)
	if len(pkce) != 1 || pkce[0] != "S256" {
		t.Fatalf("code_challenge_methods_supported must be [S256], got %v", m["code_challenge_methods_supported"])
	}
	// CIMD signalled (current spec field).
	if m["client_id_metadata_document_supported"] != true {
		t.Fatalf("client_id_metadata_document_supported must be true, got %v", m["client_id_metadata_document_supported"])
	}
	// CIMD-only: NO DCR registration endpoint advertised, ever.
	if _, present := m["registration_endpoint"]; present {
		t.Fatalf("registration_endpoint must NOT be advertised (CIMD-only)")
	}
}

func TestForwardedHeadersDriveMetadataWhenNoExternalURL(t *testing.T) {
	h := ProtectedResourceMetadataHandler(DiscoveryConfig{ExternalURL: ""})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "proxy.example")
	h.ServeHTTP(rec, r)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["resource"] != "https://proxy.example/mcp" {
		t.Fatalf("forwarded headers must drive resource, got %v", m["resource"])
	}
}

// Discovery documents must be reachable BEFORE any token exists: when mounted
// alongside an auth-wrapped MCP catch-all, the well-known paths return 200 with no
// credential while the MCP path returns 401 — and the catch-all behaviour for the
// MCP path is unchanged.
func TestRegisterDiscoveryReachableWithoutAuthCatchAllStillEnforced(t *testing.T) {
	mux := http.NewServeMux()
	RegisterDiscovery(mux, DiscoveryConfig{ExternalURL: extURL})
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	mux.Handle("/", a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	for _, p := range []string{
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s must be reachable without auth, got %d", p, rec.Code)
		}
	}
	// The MCP catch-all is still auth-enforced.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/mcp must still require auth, got %d", rec.Code)
	}
}

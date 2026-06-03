package serverurl

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWith(host string, headers map[string]string, tlsOn bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if host != "" {
		r.Host = host
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if tlsOn {
		r.TLS = &tls.ConnectionState{}
	}
	return r
}

func TestConfiguredExternalURLWins(t *testing.T) {
	// Configured value is authoritative even when forwarded headers are present
	// and disagree — the configured value is not attacker-influenced.
	r := reqWith("internal-host:8081", map[string]string{
		"X-Forwarded-Host":  "evil.example",
		"X-Forwarded-Proto": "http",
	}, false)
	base, err := Base("https://public.example", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "https://public.example" {
		t.Fatalf("configured external_url must win, got %q", base)
	}
}

func TestConfiguredExternalURLStrippedToOrigin(t *testing.T) {
	// The helper owns path construction, so a configured value carrying a path is
	// reduced to its origin.
	base, err := Base("https://public.example/mcp", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "https://public.example" {
		t.Fatalf("expected origin, got %q", base)
	}
}

func TestMalformedExternalURLErrors(t *testing.T) {
	if _, err := Base("not-a-url", nil); err == nil {
		t.Fatal("expected error for external_url without scheme/host")
	}
}

func TestForwardedHeadersFallback(t *testing.T) {
	r := reqWith("internal:8081", map[string]string{
		"X-Forwarded-Host":  "public.example",
		"X-Forwarded-Proto": "https",
	}, false)
	base, err := Base("", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "https://public.example" {
		t.Fatalf("expected forwarded-derived origin, got %q", base)
	}
}

func TestForwardedHostCommaListTakesFirst(t *testing.T) {
	r := reqWith("internal:8081", map[string]string{
		"X-Forwarded-Host": "public.example, internal.local",
	}, false)
	base, err := Base("", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "https://public.example" {
		t.Fatalf("expected first forwarded host with default https, got %q", base)
	}
}

func TestForwardedHostWithSchemeRejected(t *testing.T) {
	// A forwarded host smuggling a scheme is untrusted-malformed; fall through to
	// the request Host instead.
	r := reqWith("internal:8081", map[string]string{
		"X-Forwarded-Host": "https://evil.example",
	}, false)
	base, err := Base("", r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "http://internal:8081" {
		t.Fatalf("expected fall-through to request host, got %q", base)
	}
}

func TestRequestHostLastResortSchemeFromTLS(t *testing.T) {
	if base, _ := Base("", reqWith("localhost:8081", nil, false)); base != "http://localhost:8081" {
		t.Fatalf("plain request must yield http origin, got %q", base)
	}
	if base, _ := Base("", reqWith("localhost:8081", nil, true)); base != "https://localhost:8081" {
		t.Fatalf("TLS request must yield https origin, got %q", base)
	}
}

func TestNoResolutionErrors(t *testing.T) {
	if _, err := Base("", nil); err == nil {
		t.Fatal("expected error when nothing resolvable")
	}
}

// The resource identifier, the served endpoint, and the path-inserted PRM location
// must all be derived from MCPEndpointPath so they cannot drift.
func TestDerivedURLsSameSourced(t *testing.T) {
	const base = "https://public.example"
	if got := ResourceURL(base); got != "https://public.example/mcp" {
		t.Fatalf("resource URL = %q", got)
	}
	if got := ProtectedResourceMetadataURL(base); got != "https://public.example/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("path-inserted PRM URL = %q", got)
	}
	if got := ProtectedResourceMetadataRootURL(base); got != "https://public.example/.well-known/oauth-protected-resource" {
		t.Fatalf("root PRM URL = %q", got)
	}
	if got := AuthorizationServerMetadataURL(base); got != "https://public.example/.well-known/oauth-authorization-server" {
		t.Fatalf("AS metadata URL = %q", got)
	}
	if got := IssuerURL(base); got != base {
		t.Fatalf("issuer = %q", got)
	}
	if AuthorizeURL(base) != "https://public.example/authorize" || TokenURL(base) != "https://public.example/token" {
		t.Fatal("authorize/token URLs wrong")
	}
}

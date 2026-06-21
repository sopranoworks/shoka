package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testVerifier returns a Verifier pointed at an in-process TLS server: the SSRF
// IP policy is relaxed (loopback is the only address a test server can bind) and
// the server's self-signed cert is trusted. The real SSRF policy is exercised by
// TestVerify_SSRFBlocksLoopback and TestBlockedIP_Table instead.
func testVerifier(srv *httptest.Server, trusted ...string) *Verifier {
	v := NewVerifier(trusted)
	v.isBlockedIP = func(net.IP) bool { return false }
	v.tlsConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	return v
}

func docHandler(t *testing.T, doc ClientMetadata) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
}

func TestVerify_RejectsNonHTTPS(t *testing.T) {
	v := NewVerifier([]string{"trusted.example"})
	if _, err := v.Verify(context.Background(), "http://trusted.example/meta"); err != ErrClientIDNotHTTPS {
		t.Fatalf("want ErrClientIDNotHTTPS, got %v", err)
	}
}

func TestVerify_RejectsUntrustedDomain(t *testing.T) {
	v := NewVerifier([]string{"trusted.example"})
	if _, err := v.Verify(context.Background(), "https://evil.example/meta"); err != ErrUntrustedDomain {
		t.Fatalf("want ErrUntrustedDomain, got %v", err)
	}
}

func TestVerify_EmptyAllowlistDeniesAll(t *testing.T) {
	v := NewVerifier(nil)
	if _, err := v.Verify(context.Background(), "https://anything.example/meta"); err != ErrUntrustedDomain {
		t.Fatalf("default-deny: want ErrUntrustedDomain, got %v", err)
	}
}

func TestVerify_HappyPath(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(nil)
	defer srv.Close()
	clientID := srv.URL + "/client-meta"
	srv.Config.Handler = docHandler(t, ClientMetadata{
		ClientID:                clientID,
		ClientName:              "Test Client",
		RedirectURIs:            []string{"https://app.example/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethod: "none",
	})
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := testVerifier(srv, host)
	md, err := v.Verify(context.Background(), clientID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if md.ClientID != clientID || len(md.RedirectURIs) != 1 {
		t.Fatalf("metadata not parsed: %+v", md)
	}
}

func TestVerify_ClientIDMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	clientID := srv.URL + "/client-meta"
	srv.Config.Handler = docHandler(t, ClientMetadata{
		ClientID:     "https://someone-else.example/meta", // lies about its identity
		RedirectURIs: []string{"https://app.example/cb"},
	})
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := testVerifier(srv, host)
	if _, err := v.Verify(context.Background(), clientID); err != ErrClientIDMismatch {
		t.Fatalf("want ErrClientIDMismatch, got %v", err)
	}
}

func TestVerify_NoRedirectURIs(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	clientID := srv.URL + "/client-meta"
	srv.Config.Handler = docHandler(t, ClientMetadata{ClientID: clientID})
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := testVerifier(srv, host)
	if _, err := v.Verify(context.Background(), clientID); err != ErrNoRedirectURIs {
		t.Fatalf("want ErrNoRedirectURIs, got %v", err)
	}
}

func TestVerify_RejectsRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	}))
	defer srv.Close()
	clientID := srv.URL + "/client-meta"
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := testVerifier(srv, host)
	if _, err := v.Verify(context.Background(), clientID); err != ErrRedirectAttempted {
		t.Fatalf("want ErrRedirectAttempted, got %v", err)
	}
}

func TestVerify_TooLarge(t *testing.T) {
	big := strings.Repeat("x", (64<<10)+1024)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"x","_pad":"` + big + `"}`))
	}))
	defer srv.Close()
	clientID := srv.URL + "/client-meta"
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := testVerifier(srv, host)
	if _, err := v.Verify(context.Background(), clientID); err != ErrDocumentTooLarge {
		t.Fatalf("want ErrDocumentTooLarge, got %v", err)
	}
}

// With the REAL SSRF policy, a trusted domain that resolves to a loopback (here,
// the in-process server) is still blocked — the IP guard fires independent of the
// domain allowlist.
func TestVerify_SSRFBlocksLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	clientID := srv.URL + "/client-meta"
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	v := NewVerifier([]string{host}) // domain trusted, but...
	v.tlsConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	// ...the real isBlockedIP policy is left in place and must block the loopback.
	if _, err := v.Verify(context.Background(), clientID); err != ErrBlockedAddress {
		t.Fatalf("want ErrBlockedAddress, got %v", err)
	}
}

func TestBlockedIP_Table(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"169.254.169.254", true}, // cloud metadata service
		{"fc00::1", true},         // IPv6 ULA
		{"fe80::1", true},         // IPv6 link-local
		{"0.0.0.0", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false},
	}
	for _, c := range cases {
		got := blockedIP(net.ParseIP(c.ip))
		if got != c.blocked {
			t.Errorf("blockedIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}

func TestRedirectURIAllowed(t *testing.T) {
	cases := []struct {
		name       string
		presented  string
		registered []string
		want       bool
	}{
		{"hosted exact", "https://app.example/cb", []string{"https://app.example/cb"}, true},
		{"hosted path mismatch", "https://app.example/evil", []string{"https://app.example/cb"}, false},
		{"hosted port not ignored", "https://app.example:8443/cb", []string{"https://app.example/cb"}, false},
		{"loopback localhost ephemeral port", "http://localhost:3118/callback", []string{"http://localhost/callback"}, true},
		{"loopback 127.0.0.1 ephemeral port", "http://127.0.0.1:51999/callback", []string{"http://127.0.0.1/callback"}, true},
		{"loopback scheme mismatch", "https://localhost:3118/callback", []string{"http://localhost/callback"}, false},
		{"loopback path mismatch", "http://localhost:3118/evil", []string{"http://localhost/callback"}, false},
		{"loopback exact still ok", "http://127.0.0.1/callback", []string{"http://127.0.0.1/callback"}, true},
		{"unregistered", "https://evil.example/cb", []string{"https://app.example/cb"}, false},
	}
	for _, c := range cases {
		if got := RedirectURIAllowed(c.presented, c.registered); got != c.want {
			t.Errorf("%s: RedirectURIAllowed(%q,%v)=%v want %v", c.name, c.presented, c.registered, got, c.want)
		}
	}
}

// TestDomainTrusted_DynamicSource (B-71 Stage 2c): when a dynamic trusted source is set,
// DomainTrusted consults it (the dynamic "domain" store) instead of the static list, with the
// same exact-or-subdomain semantics. RED-shape: a deny-all source (e.g. the seed was skipped)
// trusts nothing — the regression the seed prevents.
func TestDomainTrusted_DynamicSource(t *testing.T) {
	v := NewVerifier([]string{"static.example"})
	if !v.DomainTrusted("static.example") {
		t.Fatal("static list must trust static.example before a source is set")
	}
	v.SetTrustedSource(func(host string) bool {
		return host == "dyn.example" || strings.HasSuffix(host, ".dyn.example")
	})
	if !v.DomainTrusted("dyn.example") || !v.DomainTrusted("sub.dyn.example") {
		t.Fatal("the dynamic source must make dyn.example (and subdomains) trusted")
	}
	if v.DomainTrusted("static.example") {
		t.Fatal("once a dynamic source is set, the static list is no longer consulted")
	}
	v.SetTrustedSource(func(string) bool { return false })
	if v.DomainTrusted("dyn.example") {
		t.Fatal("a deny-all source (seed skipped) must trust nothing")
	}
}

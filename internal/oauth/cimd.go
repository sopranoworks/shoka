package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CIMD (Client ID Metadata Documents, SEP-991) is the ONLY client-registration
// path Shoka supports: there is no /register and no DCR. A client presents its
// client_id as an HTTPS URL; Shoka fetches that URL, validates the returned
// metadata document, and confirms the document self-describes the same client_id.
// No client registration is ever stored — the client_id is the metadata URL.
//
// The fetch is security-critical: the client_id URL is attacker-influenced, so
// the fetch is SSRF-hardened (HTTPS-only, trusted-domain allowlist, blocked
// private/loopback/link-local/metadata addresses with the resolved IP pinned for
// the actual dial to defeat DNS rebinding, size + time caps, no redirects).

// CIMD verification errors. Each rejection has its own sentinel so the caller and
// tests can distinguish them.
var (
	ErrClientIDNotHTTPS  = errors.New("cimd: client_id must be an absolute https URL")
	ErrUntrustedDomain   = errors.New("cimd: client_id host is not in the trusted-domain allowlist")
	ErrBlockedAddress    = errors.New("cimd: client_id host resolves to a blocked address")
	ErrFetchFailed       = errors.New("cimd: could not fetch client metadata document")
	ErrRedirectAttempted = errors.New("cimd: client metadata URL attempted a redirect")
	ErrDocumentTooLarge  = errors.New("cimd: client metadata document exceeds size limit")
	ErrInvalidDocument   = errors.New("cimd: client metadata document is not valid JSON")
	ErrClientIDMismatch  = errors.New("cimd: document client_id does not match the fetched URL")
	ErrNoRedirectURIs    = errors.New("cimd: client metadata document declares no redirect_uris")
)

// ClientMetadata is the Client ID Metadata Document a client self-hosts. Field
// set per the current MCP authorization spec / OAuth client metadata (the same
// shape Claude publishes).
type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

const (
	cimdMaxBytes = 64 << 10 // 64 KiB is ample for a client metadata document
	cimdTimeout  = 5 * time.Second
)

// Verifier fetches and validates Client ID Metadata Documents. The trusted-domain
// allowlist is supplied by the operator's config (never hardcoded — confidentiality
// and flexibility). isBlockedIP is the SSRF address policy; it is a field so tests
// can relax it for an in-process server while the real policy is exercised by the
// SSRF tests.
type Verifier struct {
	trusted     []string // lowercased trusted domains from static config (seed source / fallback)
	maxBytes    int64
	timeout     time.Duration
	resolver    *net.Resolver
	isBlockedIP func(net.IP) bool
	tlsConfig   *tls.Config            // nil in production (system roots); set by tests
	trustedSrc  func(host string) bool // B-71 Stage 2c: dynamic trusted-domain source; nil ⇒ static list
}

// NewVerifier builds a Verifier trusting exactly the given client-metadata
// domains (host or parent domain; a subdomain of a trusted domain is trusted).
// An empty allowlist means default-deny: no client domain is accepted, so the
// operator MUST configure at least the legitimate connector domain(s).
func NewVerifier(trustedDomains []string) *Verifier {
	trusted := make([]string, 0, len(trustedDomains))
	for _, d := range trustedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			trusted = append(trusted, d)
		}
	}
	return &Verifier{
		trusted:     trusted,
		maxBytes:    cimdMaxBytes,
		timeout:     cimdTimeout,
		resolver:    net.DefaultResolver,
		isBlockedIP: blockedIP,
	}
}

// TrustedCount reports how many trusted client-metadata domains are configured.
// Operational logging records this COUNT (never the values) so an operator can
// tell the empty-list default-deny case from a configured-but-no-match rejection.
func (v *Verifier) TrustedCount() int {
	return len(v.trusted)
}

// SetTrustedSource switches DomainTrusted onto a dynamic trusted-domain source (B-71 Stage
// 2c): the production wiring injects oauthstore.Store.TrustedDomain, so a domain is trusted
// iff a "domain" RegistrationEntry covers it (exact-or-subdomain — same semantics as the
// static list). With no source set, the static config allowlist (NewVerifier) is used — the
// path tests exercise. The dynamic store is seeded from the static config at startup, so the
// switch preserves exactly the previously-trusted set.
func (v *Verifier) SetTrustedSource(fn func(host string) bool) {
	v.trustedSrc = fn
}

// DomainTrusted reports whether host (no port) is allowed: an exact match or a subdomain of a
// trusted entry. The dynamic source (when set) is the source of truth (B-71 Stage 2c);
// otherwise the static config allowlist. Default-deny.
func (v *Verifier) DomainTrusted(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if v.trustedSrc != nil {
		return v.trustedSrc(host)
	}
	for _, d := range v.trusted {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Verify fetches the client metadata document at clientID and validates it.
// Returns the parsed metadata on success, or one of the sentinel errors above.
func (v *Verifier) Verify(ctx context.Context, clientID string) (*ClientMetadata, error) {
	u, err := url.Parse(strings.TrimSpace(clientID))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, ErrClientIDNotHTTPS
	}
	if !v.DomainTrusted(u.Hostname()) {
		return nil, ErrUntrustedDomain
	}

	body, err := v.fetch(ctx, u)
	if err != nil {
		return nil, err
	}

	// Tolerant of unknown fields (forward-compatible: clients may add metadata).
	var md ClientMetadata
	if err := json.Unmarshal(body, &md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	// The document MUST self-describe the exact client_id it was fetched from
	// (CIMD's core integrity check — it proves the URL owner authored the doc).
	if strings.TrimSpace(md.ClientID) != clientID {
		return nil, ErrClientIDMismatch
	}
	if len(md.RedirectURIs) == 0 {
		return nil, ErrNoRedirectURIs
	}
	return &md, nil
}

// fetch performs the SSRF-hardened GET: a pinned-IP dialer (resolve once, block
// disallowed addresses, dial the validated IP so a rebinding re-resolution can't
// slip through), no redirects, and a size + time cap.
func (v *Verifier) fetch(ctx context.Context, u *url.URL) ([]byte, error) {
	transport := &http.Transport{
		DialContext:           v.safeDial,
		TLSHandshakeTimeout:   v.timeout,
		ResponseHeaderTimeout: v.timeout,
		DisableKeepAlives:     true,
		TLSClientConfig:       v.tlsConfig,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   v.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectAttempted
		},
	}
	cctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirectAttempted) {
			return nil, ErrRedirectAttempted
		}
		if errors.Is(err, ErrBlockedAddress) {
			return nil, ErrBlockedAddress
		}
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrFetchFailed, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, v.maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	if int64(len(body)) > v.maxBytes {
		return nil, ErrDocumentTooLarge
	}
	return body, nil
}

// safeDial resolves the target host, blocks the dial if ANY resolved address is
// disallowed (conservative: a host resolving to both a public and a private IP
// is treated as hostile), and dials the validated IP directly — so the IP used
// for the connection is exactly the one that passed the policy (no second
// resolution, defeating DNS rebinding).
func (v *Verifier) safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, ErrBlockedAddress
	}
	// A literal IP in the URL is checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if v.isBlockedIP(ip) {
			return nil, ErrBlockedAddress
		}
		d := &net.Dialer{Timeout: v.timeout}
		return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	ips, err := v.resolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, ErrBlockedAddress
	}
	for _, ipa := range ips {
		if v.isBlockedIP(ipa.IP) {
			return nil, ErrBlockedAddress
		}
	}
	d := &net.Dialer{Timeout: v.timeout}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// blockedIP is the real SSRF address policy: reject loopback, private (RFC 1918 +
// ULA fc00::/7), link-local (incl. the 169.254.169.254 cloud metadata service),
// unspecified, and multicast addresses.
func blockedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast()
}

// RedirectURIAllowed reports whether presented matches one of the client's
// registered redirect URIs. RFC 8252 §7.3 loopback matching is applied for
// loopback hosts ONLY (127.0.0.1 / ::1 / localhost) — the port is ignored there
// because native apps use ephemeral ports (Claude Code). For every NON-loopback
// redirect the match is byte-for-byte exact (anything looser is an open-redirect
// hole — operator constraint O3); hosted Claude uses a fixed https redirect.
func RedirectURIAllowed(presented string, registered []string) bool {
	pp, err := url.Parse(presented)
	if err != nil {
		return false
	}
	pLoop := isLoopbackHost(pp.Hostname())
	for _, reg := range registered {
		if presented == reg {
			return true
		}
		if !pLoop {
			continue // non-loopback: exact match only (already tried above)
		}
		rp, err := url.Parse(reg)
		if err != nil || !isLoopbackHost(rp.Hostname()) {
			continue
		}
		// Both loopback: match scheme, host, and path; ignore the port.
		if pp.Scheme == rp.Scheme && strings.EqualFold(pp.Hostname(), rp.Hostname()) && pp.Path == rp.Path {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether host is a loopback literal or "localhost".
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

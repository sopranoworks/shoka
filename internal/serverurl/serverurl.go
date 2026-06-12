// Package serverurl composes Shoka's public HTTPS base URL and the OAuth/MCP
// self-reference URLs derived from it. Shoka listens on plain HTTP behind a
// TLS-terminating reverse proxy, so it cannot read its own public name from the
// listen address; this package resolves it.
//
// Precedence (Base): the configured external_url wins — it is authoritative and
// not attacker-influenced. Absent that, the proxy-set X-Forwarded-Proto +
// X-Forwarded-Host are an untrusted fallback (validated for shape, used only for
// URL composition). Absent those, the request Host with the request's own scheme
// is the last resort (dev). Every OAuth URL flows through this one helper; no URL
// is assembled by ad-hoc string building elsewhere.
package serverurl

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MCPEndpointPath is the single source of truth for the path the MCP Streamable
// HTTP endpoint is served at. The PRM `resource` identifier, the served MCP
// endpoint, and the path-inserted Protected Resource Metadata location are ALL
// derived from this one constant so they can never drift (operator refinement,
// 2026-06-03): with a fixed "/mcp" they resolve to the same string, but they stay
// same-sourced rather than independently constructed.
const MCPEndpointPath = "/mcp"

// Well-known path prefixes (RFC 9728 / RFC 8414). The PRM prefix has the resource
// path component inserted after it (RFC 9728 §3.1), hence it is kept separate from
// MCPEndpointPath.
const (
	protectedResourceWellKnown   = "/.well-known/oauth-protected-resource"
	authorizationServerWellKnown = "/.well-known/oauth-authorization-server"
)

// Base resolves Shoka's public origin (scheme://host, no trailing slash) for use
// in OAuth/MCP self-references. See the package doc for precedence. It returns an
// error when no usable origin can be resolved.
func Base(configuredExternalURL string, r *http.Request) (string, error) {
	if v := strings.TrimSpace(configuredExternalURL); v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", fmt.Errorf("invalid external_url %q: must be an absolute URL with scheme and host", configuredExternalURL)
		}
		return u.Scheme + "://" + u.Host, nil
	}
	if r != nil {
		if host := firstForwardedValue(r.Header.Get("X-Forwarded-Host")); host != "" && validHost(host) {
			proto := firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))
			if proto != "http" && proto != "https" {
				proto = "https"
			}
			return proto + "://" + host, nil
		}
		if r.Host != "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			return scheme + "://" + r.Host, nil
		}
	}
	return "", fmt.Errorf("cannot resolve public base URL: external_url unset and no usable request host")
}

// firstForwardedValue returns the first comma-separated token, trimmed. Proxies
// may append (X-Forwarded-Host: outer, inner); the first is the client-facing one.
func firstForwardedValue(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// validHost rejects a forwarded host that smuggles a scheme, path, or whitespace —
// it must be a bare host[:port]. Untrusted input: shape-validated before use.
func validHost(h string) bool {
	if h == "" || strings.ContainsAny(h, "/\\ \t") || strings.Contains(h, "://") {
		return false
	}
	return true
}

// ResourceURL is the PRM `resource` identifier: the exact MCP endpoint a client
// connects to. Derived from MCPEndpointPath (single source of truth).
func ResourceURL(base string) string { return base + MCPEndpointPath }

// ProtectedResourceMetadataURL is the RFC 9728 §3.1 path-inserted PRM location for
// the MCP resource: the well-known prefix with the resource path inserted after it.
func ProtectedResourceMetadataURL(base string) string {
	return base + protectedResourceWellKnown + MCPEndpointPath
}

// ProtectedResourceMetadataRootURL is the root PRM location, the fallback clients
// probe when no path-inserted document is found and no header is present.
func ProtectedResourceMetadataRootURL(base string) string {
	return base + protectedResourceWellKnown
}

// AuthorizationServerMetadataURL is the RFC 8414 AS metadata location.
func AuthorizationServerMetadataURL(base string) string {
	return base + authorizationServerWellKnown
}

// IssuerURL is Shoka-as-AS's issuer identifier (the public origin itself).
func IssuerURL(base string) string { return base }

// AuthorizeURL / TokenURL are advertised by the AS metadata. The endpoints are
// built by directive (b); (a) only composes and advertises the URLs.
func AuthorizeURL(base string) string { return base + "/authorize" }
func TokenURL(base string) string     { return base + "/token" }

// RegistrationURL is the RFC 7591 Dynamic Client Registration endpoint advertised
// as registration_endpoint in the AS metadata (B-63). claude.ai's connector docs
// require DCR; the endpoint coexists with CIMD.
func RegistrationURL(base string) string { return base + "/register" }

// ProtectedResourceMetadataPath / AuthorizationServerMetadataPath are the request
// paths the discovery handlers mount at (path-only, for http.ServeMux).
func ProtectedResourceMetadataPath() string     { return protectedResourceWellKnown + MCPEndpointPath }
func ProtectedResourceMetadataRootPath() string { return protectedResourceWellKnown }
func AuthorizationServerMetadataPath() string   { return authorizationServerWellKnown }

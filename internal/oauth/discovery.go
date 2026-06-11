// Package oauth implements the OAuth 2.1 discovery substrate for Shoka acting as
// its own built-in authorization server (directive 2026-06-03, the (a) /
// discovery half). It serves RFC 9728 Protected Resource Metadata and RFC 8414
// Authorization Server Metadata, and provides the resource_metadata URL the auth
// middleware advertises in its 401 challenge.
//
// This is discovery ONLY: it advertises the /authorize and /token endpoints (built
// by directive (b)) and signals CIMD client identification, but issues no tokens,
// runs no consent, and validates nothing. CIMD-only — no DCR registration_endpoint
// is ever advertised. HTTP-layer only: no go-git, no refs, no persistent state.
package oauth

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/shoka/mcp-server/internal/serverurl"
)

// DiscoveryConfig configures the OAuth discovery handlers. ExternalURL is the
// configured public origin authoritative for OAuth/MCP self-references
// (Server.MCP.OAuth.ExternalURL); empty falls back to per-request forwarded headers.
type DiscoveryConfig struct {
	ExternalURL string
	// Logger records which discovery document was served (B-52), so a
	// discovery-path failure is visible. Nil → slog.Default().
	Logger *slog.Logger
}

// logger resolves the configured logger, defaulting to slog.Default() (panic-safe).
func (c DiscoveryConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// ProtectedResourceMetadata is the RFC 9728 document. resource is the exact MCP
// endpoint a client connects to; authorization_servers points at Shoka itself.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
}

// AuthorizationServerMetadata is the RFC 8414 document for Shoka-as-AS. It
// advertises PKCE S256 (mandatory), the authorization-code + refresh-token grants,
// and CIMD client identification. It deliberately omits registration_endpoint
// (CIMD-only — no DCR).
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
}

// ProtectedResourceMetadataHandler serves RFC 9728 PRM. Reachable without auth
// (discovery must work before a token exists).
func ProtectedResourceMetadataHandler(cfg DiscoveryConfig) http.Handler {
	logger := cfg.logger()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base, err := serverurl.Base(cfg.ExternalURL, r)
		if err != nil {
			logger.Warn("oauth discovery served",
				"document", "protected_resource_metadata", "result", "public-url-unresolvable")
			http.Error(w, "public URL not resolvable", http.StatusServiceUnavailable)
			return
		}
		logger.Info("oauth discovery served", "document", "protected_resource_metadata")
		writeJSON(w, ProtectedResourceMetadata{
			Resource:               serverurl.ResourceURL(base),
			AuthorizationServers:   []string{serverurl.IssuerURL(base)},
			BearerMethodsSupported: []string{"header"},
		})
	})
}

// AuthorizationServerMetadataHandler serves RFC 8414 AS metadata. Reachable
// without auth.
func AuthorizationServerMetadataHandler(cfg DiscoveryConfig) http.Handler {
	logger := cfg.logger()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base, err := serverurl.Base(cfg.ExternalURL, r)
		if err != nil {
			logger.Warn("oauth discovery served",
				"document", "authorization_server_metadata", "result", "public-url-unresolvable")
			http.Error(w, "public URL not resolvable", http.StatusServiceUnavailable)
			return
		}
		logger.Info("oauth discovery served", "document", "authorization_server_metadata")
		writeJSON(w, AuthorizationServerMetadata{
			Issuer:                            serverurl.IssuerURL(base),
			AuthorizationEndpoint:             serverurl.AuthorizeURL(base),
			TokenEndpoint:                     serverurl.TokenURL(base),
			ResponseTypesSupported:            []string{"code"},
			GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			TokenEndpointAuthMethodsSupported: []string{"none"},
			ClientIDMetadataDocumentSupported: true,
		})
	})
}

// RegisterDiscovery mounts the discovery documents on mux WITHOUT auth. The PRM is
// served at both the RFC 9728 §3.1 path-inserted location (canonical, matching the
// resource identifier) and the root location (the fallback clients probe).
func RegisterDiscovery(mux *http.ServeMux, cfg DiscoveryConfig) {
	prm := ProtectedResourceMetadataHandler(cfg)
	mux.Handle(serverurl.ProtectedResourceMetadataPath(), prm)
	mux.Handle(serverurl.ProtectedResourceMetadataRootPath(), prm)
	mux.Handle(serverurl.AuthorizationServerMetadataPath(), AuthorizationServerMetadataHandler(cfg))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

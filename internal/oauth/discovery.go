// Package oauth implements the OAuth 2.1 discovery substrate for Shoka acting as
// its own built-in authorization server (directive 2026-06-03, the (a) /
// discovery half). It serves RFC 9728 Protected Resource Metadata and RFC 8414
// Authorization Server Metadata, and provides the resource_metadata URL the auth
// middleware advertises in its 401 challenge.
//
// This is discovery ONLY: it advertises the /authorize, /token, and (in DCR mode)
// /register endpoints (built by directives (b) and B-63), but issues no tokens, runs
// no consent, and validates nothing. The advertised client-registration posture is a
// config switch (B-63 §0.1): CIMD mode (default) signals
// client_id_metadata_document_supported and NO registration_endpoint; DCR mode
// advertises registration_endpoint (RFC 7591) and WITHHOLDS the CIMD signal. The two
// CANNOT both be advertised if DCR is to be reachable — Claude's selection rule skips
// Dynamic Client Registration whenever the CIMD signal is advertised, so a
// CIMD-signalling AS is never asked to register. HTTP-layer only: no go-git, no refs,
// no persistent state.
package oauth

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/sopranoworks/shoka/internal/reqtrace"
	"github.com/sopranoworks/shoka/internal/serverurl"
)

// RegistrationMode selects which client-registration posture the AS metadata
// advertises (B-63 §0.1). Both client-resolution code paths (CIMD https-URL client_id;
// DCR issued handle) remain in the binary and /register stays mounted in either mode;
// only the advertised metadata differs, because Claude's selection rule skips DCR
// whenever the CIMD signal is advertised. The empty value is treated as CIMD.
type RegistrationMode string

const (
	// RegistrationModeCIMD (default) advertises client_id_metadata_document_supported
	// and NO registration_endpoint — today's behaviour; claude.ai selects CIMD.
	RegistrationModeCIMD RegistrationMode = "cimd"
	// RegistrationModeDCR advertises registration_endpoint (RFC 7591) and WITHHOLDS
	// client_id_metadata_document_supported, so Claude's selection rule lands on DCR
	// and POSTs its client metadata to /register. token_endpoint_auth_methods_supported
	// stays ["none"] (the DCR client is still public).
	RegistrationModeDCR RegistrationMode = "dcr"
)

// DiscoveryConfig configures the OAuth discovery handlers. ExternalURL is the
// configured public origin authoritative for OAuth/MCP self-references
// (Server.MCP.OAuth.ExternalURL); empty falls back to per-request forwarded headers.
type DiscoveryConfig struct {
	ExternalURL string
	// RegistrationMode selects the advertised registration posture (B-63 §0.1). The
	// empty value (and anything other than "dcr") is treated as CIMD — the default.
	RegistrationMode RegistrationMode
	// Logger records which discovery document was served (B-52), so a
	// discovery-path failure is visible. Nil → slog.Default().
	Logger *slog.Logger
}

// advertiseDCR reports whether AS metadata should advertise the DCR posture
// (registration_endpoint present, CIMD signal withheld). Only the explicit "dcr"
// value selects DCR; every other value (incl. empty and "cimd") is CIMD.
func (c DiscoveryConfig) advertiseDCR() bool {
	return c.RegistrationMode == RegistrationModeDCR
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
// advertises PKCE S256 (mandatory) and the authorization-code + refresh-token grants,
// plus EITHER the CIMD signal OR (B-63) the RFC 7591 Dynamic Client Registration
// endpoint — never both, selected by DiscoveryConfig.RegistrationMode (§0.1).
// registration_endpoint and client_id_metadata_document_supported are both omitempty
// so exactly the active posture's field is emitted.
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported,omitempty"`
}

// ProtectedResourceMetadataHandler serves RFC 9728 PRM. Reachable without auth
// (discovery must work before a token exists).
func ProtectedResourceMetadataHandler(cfg DiscoveryConfig) http.Handler {
	logger := cfg.logger()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lg := logger.With("request_id", reqtrace.ID(r.Context()))
		base, err := serverurl.Base(cfg.ExternalURL, r)
		if err != nil {
			lg.Warn("oauth discovery served",
				"document", "protected_resource_metadata", "result", "public-url-unresolvable")
			http.Error(w, "public URL not resolvable", http.StatusServiceUnavailable)
			return
		}
		lg.Info("oauth discovery served", "document", "protected_resource_metadata")
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
		lg := logger.With("request_id", reqtrace.ID(r.Context()))
		base, err := serverurl.Base(cfg.ExternalURL, r)
		if err != nil {
			lg.Warn("oauth discovery served",
				"document", "authorization_server_metadata", "result", "public-url-unresolvable")
			http.Error(w, "public URL not resolvable", http.StatusServiceUnavailable)
			return
		}
		md := AuthorizationServerMetadata{
			Issuer:                        serverurl.IssuerURL(base),
			AuthorizationEndpoint:         serverurl.AuthorizeURL(base),
			TokenEndpoint:                 serverurl.TokenURL(base),
			ResponseTypesSupported:        []string{"code"},
			GrantTypesSupported:           []string{"authorization_code", "refresh_token"},
			CodeChallengeMethodsSupported: []string{"S256"},
			// B-71 Stage 3: the token endpoint now also authenticates confidential pre-issued
			// clients (Client ID + Secret) via client_secret_basic / client_secret_post, IN ADDITION
			// to PKCE. "none" stays — public clients (CIMD/DCR/self) still exist.
			TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_basic", "client_secret_post"},
		}
		// B-63 §0.1: advertise EXACTLY one registration posture. DCR mode →
		// registration_endpoint, CIMD signal withheld; CIMD mode (default) → CIMD
		// signal, registration_endpoint withheld. The two cannot coexist or Claude
		// skips DCR. token_endpoint_auth_methods_supported stays ["none"] in both.
		mode := "cimd"
		if cfg.advertiseDCR() {
			md.RegistrationEndpoint = serverurl.RegistrationURL(base)
			mode = "dcr"
		} else {
			md.ClientIDMetadataDocumentSupported = true
		}
		lg.Info("oauth discovery served", "document", "authorization_server_metadata", "registration_mode", mode)
		writeJSON(w, md)
	})
}

// RegisterDiscovery mounts the discovery documents on mux WITHOUT auth. The PRM is
// served at both the RFC 9728 §3.1 path-inserted location (canonical, matching the
// resource identifier) and the root location (the fallback clients probe).
func RegisterDiscovery(mux *http.ServeMux, cfg DiscoveryConfig) {
	// reqtrace.Route tags the routing stage (B-53 §2.5) so a discovery request's
	// response line names route="oauth-discovery" under the shared request_id.
	prm := reqtrace.Route("oauth-discovery", ProtectedResourceMetadataHandler(cfg))
	mux.Handle(serverurl.ProtectedResourceMetadataPath(), prm)
	mux.Handle(serverurl.ProtectedResourceMetadataRootPath(), prm)
	mux.Handle(serverurl.AuthorizationServerMetadataPath(), reqtrace.Route("oauth-discovery", AuthorizationServerMetadataHandler(cfg)))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

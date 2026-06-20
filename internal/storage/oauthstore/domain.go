package oauthstore

import (
	"net/url"
	"strings"
)

// B-71 Stage 2a — domain attribution. Every token series must be attributable to a domain
// so later stages can group tokens under a "domain" registration entry and apply per-domain
// TTL/consent. A CIMD series is already domain-attributable (its ClientID is the metadata
// URL, domain = host). A DCR series gets a recorded domain (RegisteredClient.Domain, derived
// from its redirect_uris host at /register). The operator self-issued token is neither.
//
// This is RECORD-ONLY: nothing here gates authorization or changes display yet (Stages
// 2b/2c/2d).

// cimdHost returns the host of a CIMD client_id (an absolute https URL), lowercased and
// port-stripped, or "" if clientID is not an https URL (i.e. a DCR opaque handle or the
// self-issued marker). This is the inverse of oauth.isDCRClientID, kept here so oauthstore
// has no dependency on package oauth (which imports oauthstore).
func cimdHost(clientID string) string {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// DomainFromRedirectURIs derives a DCR client's domain from its redirect_uris: the single
// shared host (lowercased, port-stripped) when every parseable redirect_uri has the SAME
// host, otherwise "" (a multi-host or unparseable client is left unattributed — recorded as
// "" and re-derived lazily). It NEVER rejects: Stage 2a only records a domain, it does not
// gate registration (that trust enforcement is deferred to Stage 2c).
func DomainFromRedirectURIs(redirectURIs []string) string {
	host := ""
	for _, ru := range redirectURIs {
		u, err := url.Parse(strings.TrimSpace(ru))
		if err != nil || u.Host == "" {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if h == "" {
			continue
		}
		if host == "" {
			host = h
			continue
		}
		if h != host {
			return "" // multiple distinct hosts ⇒ unattributed (no single domain)
		}
	}
	return host
}

// SeriesDomain reports the trusted domain a series belongs to, its registration procedure,
// and whether it is domain-attributable — the single seam Stage 2d groups by and Stages
// 2b/2c apply per-domain policy through. For:
//   - a CIMD series: (host of the ClientID metadata URL, "cimd", true);
//   - a DCR series: (the RegisteredClient.Domain, or a lazy derive from its redirect_uris
//     for a pre-Stage-2a record, "dcr", true) — (..., false) if the client is gone or has no
//     attributable domain;
//   - the operator self-issued "shoka-cli" series: ("", "", false) — not a domain.
//
// Procedure is determined by the ClientID shape (an https URL ⇒ cimd; an opaque handle ⇒
// dcr), mirroring oauth.isDCRClientID. RECORD-ONLY: no caller gates/groups on this yet.
func (s *Store) SeriesDomain(rec SeriesRecord) (domain, procedure string, ok bool) {
	if rec.ClientID == SelfIssuedClientID {
		return "", "", false // operator self-issued — belongs to the confidential world, not a domain
	}
	if h := cimdHost(rec.ClientID); h != "" {
		return h, "cimd", true // CIMD: client_id is the metadata URL, domain = its host
	}
	// DCR: the opaque client_id resolves to a RegisteredClient carrying the recorded domain.
	client, err := s.GetClient(rec.ClientID)
	if err != nil {
		return "", "", false
	}
	d := client.Domain
	if d == "" {
		d = DomainFromRedirectURIs(client.RedirectURIs) // lazy for a pre-Stage-2a record
	}
	if d == "" {
		return "", "", false
	}
	return d, "dcr", true
}

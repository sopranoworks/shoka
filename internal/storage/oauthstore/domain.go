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
// dcr), mirroring oauth.isDCRClientID.
func (s *Store) SeriesDomain(rec SeriesRecord) (domain, procedure string, ok bool) {
	return s.clientDomain(rec.ClientID)
}

// clientDomain is the core of SeriesDomain, keyed on a client_id: CIMD ⇒ (URL host, "cimd");
// DCR ⇒ (recorded RegisteredClient.Domain, else lazy from redirect_uris, "dcr"); the operator
// self-issued client ⇒ ("","",false). It backs both SeriesDomain (per-series grouping) and
// DomainEntryForClient (per-domain consent/TTL at /authorize and /token, B-71 Stage 2c).
func (s *Store) clientDomain(clientID string) (domain, procedure string, ok bool) {
	if clientID == SelfIssuedClientID {
		return "", "", false // operator self-issued — belongs to the confidential world, not a domain
	}
	if h := cimdHost(clientID); h != "" {
		return h, "cimd", true // CIMD: client_id is the metadata URL, domain = its host
	}
	// DCR: the opaque client_id resolves to a RegisteredClient carrying the recorded domain.
	client, err := s.GetClient(clientID)
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

// DomainEntryForHost returns the "domain" RegistrationEntry whose Identifier is host or a
// parent domain of host (exact-or-subdomain — preserving the pre-B-71 static-allowlist
// semantics), and whether one was found. On multiple matches the most specific (longest
// identifier) wins. B-71 Stage 2c: the dynamic store is the source of truth.
func (s *Store) DomainEntryForHost(host string) (RegistrationEntry, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return RegistrationEntry{}, false
	}
	entries, err := s.ListRegistrations()
	if err != nil {
		return RegistrationEntry{}, false
	}
	var best RegistrationEntry
	found := false
	for _, e := range entries {
		if e.RegistrationMode != RegistrationModeDomain {
			continue
		}
		d := strings.ToLower(strings.TrimSpace(e.Identifier))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			if !found || len(d) > len(best.Identifier) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// TrustedDomain reports whether host is covered by a "domain" RegistrationEntry (exact or
// subdomain). The CIMD verifier and the DCR /register trust gate consult this instead of the
// static trusted_client_metadata_domains list (B-71 Stage 2c). Default-deny when no entry
// matches.
func (s *Store) TrustedDomain(host string) bool {
	_, ok := s.DomainEntryForHost(host)
	return ok
}

// DomainEntryForClient returns the "domain" RegistrationEntry a client_id belongs to — the
// seam the /authorize consent gate (VerifyConsent) and /token issuance (EffectiveTTL) use to
// apply per-domain policy (B-71 Stage 2c). false when the client has no domain (operator
// self-issued) or no matching domain entry.
func (s *Store) DomainEntryForClient(clientID string) (RegistrationEntry, bool) {
	domain, _, ok := s.clientDomain(clientID)
	if !ok {
		return RegistrationEntry{}, false
	}
	return s.DomainEntryForHost(domain)
}

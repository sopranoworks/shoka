package oauthstore

import (
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// B-71 Stage 2a: DCR domain attribution + the SeriesDomain helper.

func TestDomainFromRedirectURIs(t *testing.T) {
	cases := []struct {
		name string
		uris []string
		want string
	}{
		{"single host", []string{"https://app.example/cb"}, "app.example"},
		{"port stripped + lowercased", []string{"https://App.Example:8443/cb"}, "app.example"},
		{"multiple same host", []string{"https://app.example/a", "https://app.example/b"}, "app.example"},
		{"distinct hosts ⇒ unattributed", []string{"https://a.example/cb", "https://b.example/cb"}, ""},
		{"none", nil, ""},
		{"unparseable only", []string{"::::"}, ""},
		{"no-host scheme skipped", []string{"myapp:/cb"}, ""},
	}
	for _, c := range cases {
		if got := DomainFromRedirectURIs(c.uris); got != c.want {
			t.Errorf("%s: DomainFromRedirectURIs(%v) = %q, want %q", c.name, c.uris, got, c.want)
		}
	}
}

// A RegisteredClient.Domain round-trips, and a pre-Stage-2a record (no "domain" key) decodes
// safely to "" — no breaking migration.
func TestRegisteredClient_DomainRoundTripAndDecodeSafe(t *testing.T) {
	s := openTemp(t)
	if err := s.PutClient(RegisteredClient{ClientID: "h1", RedirectURIs: []string{"https://app.example/cb"}, Domain: "app.example"}); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	got, err := s.GetClient("h1")
	if err != nil || got.Domain != "app.example" {
		t.Fatalf("domain must round-trip: %+v err=%v", got, err)
	}
	// A record written before Stage 2a (no "domain" key) decodes with Domain == "".
	old := `{"client_id":"old","redirect_uris":["https://old.example/cb"],"token_endpoint_auth_method":"none","client_id_issued_at":"2026-06-01T00:00:00Z"}`
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(clientsBucket)).Put([]byte("old"), []byte(old))
	}); err != nil {
		t.Fatalf("seed old client: %v", err)
	}
	o, err := s.GetClient("old")
	if err != nil || o.Domain != "" {
		t.Fatalf("pre-Stage-2a record must decode with empty Domain: %+v err=%v", o, err)
	}
}

// TestSeriesDomain covers all three procedures. RED proof: break cimdHost (return the raw
// client_id) → the CIMD case returns the wrong domain; or skip the recorded-domain read in
// SeriesDomain → the DCR case returns the redirect host instead of the recorded domain.
func TestSeriesDomain(t *testing.T) {
	s := openTemp(t)

	// CIMD: client_id is the metadata URL; domain = its host (lowercased, port-stripped).
	d, proc, ok := s.SeriesDomain(SeriesRecord{ClientID: "https://Connector.Example:443/meta"})
	if !ok || d != "connector.example" || proc != "cimd" {
		t.Fatalf("CIMD: got (%q, %q, %v), want (connector.example, cimd, true)", d, proc, ok)
	}

	// DCR with a RECORDED domain that differs from the redirect host — proves the recorded
	// Domain is the source of truth (not a lazy redirect derive).
	dcrID, _ := NewHandle()
	if err := s.PutClient(RegisteredClient{ClientID: dcrID, RedirectURIs: []string{"https://redirect.example/cb"}, Domain: "recorded.example"}); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	d, proc, ok = s.SeriesDomain(SeriesRecord{ClientID: dcrID})
	if !ok || d != "recorded.example" || proc != "dcr" {
		t.Fatalf("DCR recorded: got (%q, %q, %v), want (recorded.example, dcr, true)", d, proc, ok)
	}

	// DCR with NO recorded domain (pre-Stage-2a) — derived lazily from redirect_uris.
	lazyID, _ := NewHandle()
	if err := s.PutClient(RegisteredClient{ClientID: lazyID, RedirectURIs: []string{"https://lazy.example/cb"}}); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	d, proc, ok = s.SeriesDomain(SeriesRecord{ClientID: lazyID})
	if !ok || d != "lazy.example" || proc != "dcr" {
		t.Fatalf("DCR lazy: got (%q, %q, %v), want (lazy.example, dcr, true)", d, proc, ok)
	}

	// DCR with no recorded domain AND multi-host redirect_uris ⇒ unattributed.
	multiID, _ := NewHandle()
	if err := s.PutClient(RegisteredClient{ClientID: multiID, RedirectURIs: []string{"https://a.example/cb", "https://b.example/cb"}}); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	if _, _, ok := s.SeriesDomain(SeriesRecord{ClientID: multiID}); ok {
		t.Fatal("a multi-host DCR client with no recorded domain must be unattributed")
	}

	// DCR opaque handle with no registered client (client gone) ⇒ unattributed.
	if _, _, ok := s.SeriesDomain(SeriesRecord{ClientID: "opaque-handle-with-no-client"}); ok {
		t.Fatal("a DCR series whose client is gone must be unattributed")
	}

	// Operator self-issued ⇒ not a domain.
	if _, _, ok := s.SeriesDomain(SeriesRecord{ClientID: SelfIssuedClientID}); ok {
		t.Fatal("the operator self-issued series must be unattributed (not a domain)")
	}
}

// B-71 Stage 2c: TrustedDomain + DomainEntryForHost/ForClient read the dynamic "domain" store.

func TestTrustedDomain_AndEntryForHost(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.CreateRegistration(RegistrationModeDomain, "trusted.example", now); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A non-domain (confidential) entry must NOT make a host trusted.
	if _, err := s.CreateRegistration(RegistrationModeConfidential, "conf.example", now); err != nil {
		t.Fatalf("create conf: %v", err)
	}
	// Exact + subdomain trusted; unrelated + the confidential identifier not.
	for host, want := range map[string]bool{
		"trusted.example":          true,
		"sub.trusted.example":      true,
		"deep.sub.trusted.example": true,
		"nottrusted.example":       false,
		"trusted.example.evil":     false,
		"conf.example":             false, // confidential entry is not a trusted domain
		"":                         false,
	} {
		if got := s.TrustedDomain(host); got != want {
			t.Errorf("TrustedDomain(%q) = %v, want %v", host, got, want)
		}
	}
	// Most-specific entry wins.
	if _, err := s.CreateRegistration(RegistrationModeDomain, "sub.trusted.example", now); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	e, ok := s.DomainEntryForHost("x.sub.trusted.example")
	if !ok || e.Identifier != "sub.trusted.example" {
		t.Fatalf("most-specific match: got %q ok=%v", e.Identifier, ok)
	}
}

func TestDomainEntryForClient(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	dom, err := s.CreateRegistration(RegistrationModeDomain, "connector.example", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// CIMD client_id (the metadata URL) → the entry covering its host.
	if e, ok := s.DomainEntryForClient("https://connector.example/meta"); !ok || e.ID != dom.ID {
		t.Fatalf("CIMD client must resolve to its domain entry: ok=%v id=%q", ok, e.ID)
	}
	// DCR client whose recorded domain matches.
	dcrID, _ := NewHandle()
	if err := s.PutClient(RegisteredClient{ClientID: dcrID, RedirectURIs: []string{"https://connector.example/cb"}, Domain: "connector.example"}); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	if e, ok := s.DomainEntryForClient(dcrID); !ok || e.ID != dom.ID {
		t.Fatalf("DCR client must resolve to its domain entry: ok=%v id=%q", ok, e.ID)
	}
	// A client whose domain has no entry → not found.
	if _, ok := s.DomainEntryForClient("https://unknown.example/meta"); ok {
		t.Fatal("a client whose domain has no entry must not resolve")
	}
	// Operator self-issued → not found.
	if _, ok := s.DomainEntryForClient(SelfIssuedClientID); ok {
		t.Fatal("the self-issued client must not resolve to a domain entry")
	}
}

// TestRevokeByDomain (B-71 Stage 2d): revokes series under a domain (exact + subdomain), leaves
// other domains' and self-issued series alone.
func TestRevokeByDomain(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	p := Principal{Name: "Op"}
	a, _ := s.NewSeries("https://connector.example/meta", p, "r", "*", now, time.Hour, time.Hour)     // under connector.example
	b, _ := s.NewSeries("https://sub.connector.example/meta", p, "r", "*", now, time.Hour, time.Hour) // subdomain, under it
	other, _ := s.NewSeries("https://elsewhere.example/meta", p, "r", "*", now, time.Hour, time.Hour) // different domain
	self, _ := s.NewSeries(SelfIssuedClientID, p, "r", "*", now, time.Hour, time.Hour)                // no domain
	n, err := s.RevokeByDomain("connector.example")
	if err != nil {
		t.Fatalf("RevokeByDomain: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked = %d, want 2 (exact + subdomain)", n)
	}
	if _, err := s.Lookup(a.AccessToken, now); err == nil {
		t.Fatal("exact-domain series must be revoked")
	}
	if _, err := s.Lookup(b.AccessToken, now); err == nil {
		t.Fatal("subdomain series must be revoked")
	}
	if _, err := s.Lookup(other.AccessToken, now); err != nil {
		t.Fatal("other-domain series must survive")
	}
	if _, err := s.Lookup(self.AccessToken, now); err != nil {
		t.Fatal("self-issued series must survive")
	}
}

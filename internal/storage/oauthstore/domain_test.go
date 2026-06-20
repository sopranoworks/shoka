package oauthstore

import (
	"testing"

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

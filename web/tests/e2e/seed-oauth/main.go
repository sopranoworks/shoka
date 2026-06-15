// Command seed-oauth is an E2E-ONLY fixture tool for the OAuth connection
// management view (the 2026-06-03 MCP OAuth (c) directive). It opens an oauth.db
// and inserts one token series so the management view has a real connection to
// list and revoke end-to-end.
//
// Why a direct seed (not a real OAuth flow): the CIMD verifier's SSRF guard
// blocks loopback/private addresses, so a client-metadata document cannot be
// served from the test process on localhost — a genuine /authorize flow is
// impossible against the e2e server. Seeding the store directly, BEFORE the
// server starts (so the bbolt single-writer lock is free), is the fixture path
// the directive's §4 step 5 anticipates.
//
// It changes NOTHING in the backend: it uses only the existing oauthstore public
// API (NewSeries). The client_id is an RFC 2606 placeholder — no real client
// domain is ever written down (directive §0(b)).
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seed-oauth <oauth.db path>")
		os.Exit(2)
	}
	store, err := oauthstore.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	rec, err := store.NewSeries(
		"https://connector.example.com/.well-known/oauth-client-metadata",
		oauthstore.Principal{Name: "Op Erator", Email: "op@example.test"},
		"https://shoka.example/mcp",
		"*",
		time.Now(),
		time.Hour,
		24*time.Hour,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	fmt.Println(rec.SeriesID)
}

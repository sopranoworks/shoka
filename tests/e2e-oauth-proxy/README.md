# B-61 — Docker multi-host OAuth + MCP end-to-end harness

A local reproduction of the production posture **Cloudflare → reverse proxy →
Shoka**, built so the complete proxied OAuth 2.1 + PKCE + MCP connect can be
exercised — and any spec deviation caught — **without claude.ai and without
restart/reconnect cycles**.

Shoka's OAuth/MCP transport had never been connection-tested by a real OAuth
client over a real reverse-proxy + TLS path: in-process tests pass while the live
proxied flow was only ever exercised by pointing claude.ai at it and reading logs.
This harness closes that gap.

## What it runs

Three **separate** containers on one private Docker network (not a localhost
collapse):

| Container | Role |
|-----------|------|
| `proxy`   | **Apache httpd** (`mod_proxy`/`mod_proxy_http`/`mod_ssl`) terminating **TLS** with a local-CA cert, forwarding **plain HTTP** to `shoka:8082`, sending the real `Host` / `X-Forwarded-For` / `X-Forwarded-Proto: https` / `X-Forwarded-Host` headers. Two vhosts: `shoka.test` (the public OAuth/MCP origin) and `client.test` (serves the client's CIMD metadata document). |
| `shoka`   | The Shoka server built **from source at HEAD**, OAuth MCP transport only, `external_url = https://shoka.test`, `server.debug.dump_http: true` (the B-59 verbatim dump shows exactly what crosses the proxy). The local CA is installed so Shoka's outbound CIMD fetch over TLS verifies. |
| `client`  | A strict OAuth 2.1 + PKCE-S256 + MCP test driver. Its **exit code is the end-to-end assertion**. |

The client drives the COMPLETE flow against the public TLS proxy URL, exactly the
way claude.ai does — and one step further, to the authenticated initialize
claude.ai never reaches. It proves **both** client-registration paths the AS
advertises: **CIMD** (`client_id` = an https metadata URL) and **DCR** (RFC 7591,
**B-63** — `POST` client metadata to `registration_endpoint`, receive an opaque
public `client_id`). claude.ai's connector docs *require* DCR, so the DCR path is
the one the live connect uses.

1. unauthenticated MCP `initialize` probe → **401 + `WWW-Authenticate: Bearer
   resource_metadata="…"`**
2. discovery: Protected Resource Metadata + Authorization Server Metadata
   (asserts both `registration_endpoint` **and** `client_id_metadata_document_supported` — they coexist)
3. *(DCR path only)* register: `POST` client metadata → **opaque public `client_id`
   (no secret, `token_endpoint_auth_method: none`)**
4. authorize: `client_id` + consent → capture the code from the 302
5. token: `code` + PKCE verifier → **strict parse** of the response (Content-Type
   incl. charset, `token_type`, `Cache-Control: no-store`, JSON fields) + refresh rotation
6. **authenticated MCP `initialize` with the bearer token → `tools/list` →
   `tools/call`** round-trip, all through the TLS reverse proxy

Steps 4–6 run once per path (CIMD, then DCR). **Without `/register` the DCR path
cannot proceed** (no `registration_endpoint` advertised), so the harness fails —
the B-63 "fail without `/register`, pass with it" bar.

## Run it

```bash
cd tests/e2e-oauth-proxy
./run.sh            # build + run; exit code is the client's PASS/FAIL verdict
./run.sh logs       # same, plus print the shoka container log (B-59 dump) at the end
```

`run.sh` generates a local test CA (idempotent), then
`docker compose up --build --exit-code-from client`. Exit `0` = the full proxied
OAuth + MCP connect works end-to-end; non-zero = the failing step is named on the
`[FAIL]` line (re-run with `logs` for the verbatim dump of what crossed the proxy).

### Requirements
- Docker + Docker Compose (the harness builds three images and one network).
- Internet access to pull the `httpd:2.4` and `debian:bookworm-slim` base images
  and the Go module/toolchain (the `golang` builder). In a restricted network,
  pre-pull the bases from a mirror, e.g.
  `docker pull mirror.gcr.io/library/httpd:2.4 && docker tag mirror.gcr.io/library/httpd:2.4 httpd:2.4`.

## Why the network uses `192.0.2.0/24` (TEST-NET-1)

Shoka's CIMD fetch is SSRF-hardened: `internal/oauth/cimd.go`'s `blockedIP`
rejects loopback / RFC-1918 private / link-local addresses. A default Docker
bridge (172.x) would make Shoka's fetch of the client's metadata document fail
with `ErrBlockedAddress` — whereas real claude.ai works because its CIMD document
sits on a **public** IP. Go's `net.IP.IsPrivate()` returns **false** for the
documentation range `192.0.2.0/24`, so the network uses it: the **real, unmodified**
SSRF policy runs and passes exactly as it would for a genuinely public CIMD host.
This is a harness network choice, not a relaxation of the product's SSRF guard.

## Reproduce-then-fix

The harness is a real reproduce-then-fix instrument, not a rubber stamp. To see
it catch a post-token defect, temporarily break the token response in
`internal/oauth/server.go` (`writeTokens`) — e.g. set the Content-Type to
`text/plain` — and re-run: step 4 fails with
`STRICT token-response Content-Type rejected …`. Revert and it passes again.

At HEAD the full proxied connect **passes**: the strict client accepts Shoka's
discovery, token response, and the authenticated MCP session round-trips through
the proxy. See `reports/2026-06-12-shoka-b61-docker-multihost-oauth-e2e-test-complete.md`.

## Confidentiality

Local test CA + placeholder hostnames (`shoka.test`, `client.test`) + a throwaway
consent credential only. No operator-deployment hostname/IP/secret is committed.
The generated CA key and certs (`certs/*.key`, `certs/*.crt`) are git-ignored and
regenerated by `certs/gen-certs.sh`.

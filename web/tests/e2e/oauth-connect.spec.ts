import { test, expect } from '@playwright/test'
import crypto from 'node:crypto'

// B-71 Stage 2c — the real-browser proof of the live OAuth connect flow AFTER the switch onto
// the dynamic "domain" store. The auth server (verifier/consent/TTL) now reads the dynamic
// store, seeded at startup from the static config (trusted_client_metadata_domains "example.com"
// + the consent_credential carried into that domain). These specs drive the REAL server-rendered
// /authorize consent page in a real browser and exchange at /token — NOT page.goto shortcuts.
//
// Coverage split (a standing finding, not a shortcut): the CIMD connect cannot be hermetically
// real-browser-tested because the AS fetches the client-metadata document over HTTPS with SSRF
// hardening (loopback/private blocked, no relax knob in the shipped binary) — CIMD's live-switch
// is covered by Go regression (verifier reads the store; in-package SSRF relaxation, never
// shipped). DCR + operator self-issued (below) are covered here through the real browser.

const OAUTH_PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099) - 2
const OAUTH_BASE = `http://localhost:${OAUTH_PORT}`
const CONSENT = 'e2e-consent-secret' // matches global-setup.ts

function b64url(b: Buffer): string {
  return b.toString('base64url')
}

// An MCP initialize request; only the AUTH outcome matters here (a missing/invalid bearer is
// rejected 401 by the auth middleware BEFORE MCP processing).
function mcpInitialize(bearer?: string) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Accept: 'application/json, text/event-stream',
  }
  if (bearer) headers.Authorization = `Bearer ${bearer}`
  return {
    headers,
    data: {
      jsonrpc: '2.0',
      id: 1,
      method: 'initialize',
      params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'e2e', version: '0' } },
    },
  }
}

test('DCR client connects through the real /authorize consent page + /token, then authorizes MCP', async ({
  page,
  request,
}) => {
  // Discover the RFC 8707 resource (RFC 9728 protected-resource metadata).
  const prm = await request.get(`${OAUTH_BASE}/.well-known/oauth-protected-resource/mcp`)
  expect(prm.ok(), await prm.text()).toBeTruthy()
  const resource = (await prm.json()).resource as string
  expect(resource).toBeTruthy()

  // Register a DCR client whose redirect host (app.example.com) is a subdomain of the seeded
  // trusted "example.com" — so the Stage 2c DCR gate ADMITS it. (Skip the startup seed and this
  // 201 becomes a 400 — the seed-skip regression the switch must not introduce.)
  const redirectURI = 'https://app.example.com/cb'
  const reg = await request.post(`${OAUTH_BASE}/register`, {
    headers: { 'Content-Type': 'application/json' },
    data: {
      redirect_uris: [redirectURI],
      token_endpoint_auth_method: 'none',
      grant_types: ['authorization_code', 'refresh_token'],
      response_types: ['code'],
      client_name: 'E2E DCR client',
    },
  })
  expect(reg.status(), `register: ${await reg.text()}`).toBe(201)
  const clientID = (await reg.json()).client_id as string

  // PKCE (S256).
  const codeVerifier = b64url(crypto.randomBytes(32))
  const codeChallenge = b64url(crypto.createHash('sha256').update(codeVerifier).digest())

  // Don't leave the browser for the real internet: intercept the redirect host and serve a stub,
  // so the post-consent redirect to app.example.com resolves locally and we can read the code.
  await page.route(/^https:\/\/app\.example\.com\//, (route) =>
    route.fulfill({ status: 200, contentType: 'text/html', body: '<html>cb</html>' }),
  )

  // Drive the REAL server-rendered consent page: render, fill the consent credential, approve.
  const authURL =
    `${OAUTH_BASE}/authorize?` +
    new URLSearchParams({
      client_id: clientID,
      redirect_uri: redirectURI,
      response_type: 'code',
      code_challenge: codeChallenge,
      code_challenge_method: 'S256',
      resource,
      state: 'e2e-state',
    }).toString()
  await page.goto(authURL)
  await expect(page.locator('input[name="consent_credential"]')).toBeVisible()
  await page.fill('input[name="consent_credential"]', CONSENT)
  const [redirectReq] = await Promise.all([
    page.waitForRequest(/app\.example\.com\/cb/),
    page.click('button[name="approve"]'),
  ])

  // The post-consent redirect carried the authorization code (and the state echoed).
  const redirected = new URL(redirectReq.url())
  expect(redirected.searchParams.get('state')).toBe('e2e-state')
  const code = redirected.searchParams.get('code')
  expect(code, 'authorization code in the redirect').toBeTruthy()

  // Exchange the code at /token (the client acting).
  const tok = await request.post(`${OAUTH_BASE}/token`, {
    form: {
      grant_type: 'authorization_code',
      code: code!,
      client_id: clientID,
      redirect_uri: redirectURI,
      code_verifier: codeVerifier,
    },
  })
  expect(tok.status(), `token: ${await tok.text()}`).toBe(200)
  const accessToken = (await tok.json()).access_token as string
  expect(accessToken).toBeTruthy()

  // The issued token actually AUTHORIZES a real MCP call (and a missing bearer is 401).
  const unauth = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize())
  expect(unauth.status(), 'no bearer must be 401').toBe(401)
  const authed = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize(accessToken))
  expect(authed.status(), `bearer must authorize (not 401): ${await authed.text()}`).not.toBe(401)
})

test('operator self-issued token (Generate CLI token, via the real UI) authorizes MCP', async ({
  page,
  request,
}) => {
  // The OAuth management settings item (the old /admin/connections redirects here).
  await page.goto('/settings?item=oauth')
  await page.getByRole('button', { name: 'Generate a token for the CLI' }).click()

  // The freshly minted token is shown once in the panel — the long base64url code (not the
  // short "shoka-cli auth" hint).
  const codes = page.locator('[role="status"] code')
  await expect(codes.first()).toBeVisible()
  const texts = await codes.allTextContents()
  const token = texts.find((t) => t.length >= 40 && !t.includes(' '))
  expect(token, 'a minted token is shown once').toBeTruthy()

  const unauth = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize())
  expect(unauth.status()).toBe(401)
  const authed = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize(token!))
  expect(authed.status(), `self-issued bearer must authorize (not 401): ${await authed.text()}`).not.toBe(401)
})

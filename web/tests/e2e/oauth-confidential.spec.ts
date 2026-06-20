import { test, expect } from '@playwright/test'
import crypto from 'node:crypto'

// B-71 Stage 3 — the real-browser, through-UI proof of the confidential-client (Client ID +
// Secret) pre-issued access mode. The super-user (single-user admin) issues a confidential
// client from the OAuth settings screen (the secret shown ONCE), then a confidential connect
// authenticates at /token with client_id + secret + PKCE and the issued token authorizes a real
// MCP call; revoking the client through the UI cuts that token. NOT page.goto for the issue UI.

const OAUTH_PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099) - 2
const OAUTH_BASE = `http://localhost:${OAUTH_PORT}`

function b64url(b: Buffer): string {
  return b.toString('base64url')
}

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

test('issue a confidential client through the UI, connect with id+secret+PKCE, then revoke', async ({
  page,
  request,
}) => {
  // 1. Issue a confidential client through the real settings screen (secret shown once).
  await page.goto('/settings?item=oauth')
  await page.getByTestId('client-issue-scope').fill('*') // all-access so the token authorizes a real call
  await page.getByTestId('client-issue-validity').fill('30')
  await page.getByTestId('client-issue-submit').click()

  await expect(page.getByTestId('client-issued-panel')).toBeVisible()
  const clientID = ((await page.getByTestId('client-issued-id').textContent()) ?? '').trim()
  const secret = ((await page.getByTestId('client-issued-secret').textContent()) ?? '').trim()
  expect(clientID, 'an issued client_id is shown').toBeTruthy()
  expect(secret, 'the secret is shown once at issuance').toBeTruthy()

  // The secret is NOT shown again: reload, the client is listed but the secret is nowhere.
  await page.goto('/settings?item=oauth')
  await expect(page.getByTestId(`client-row-${clientID}`)).toBeVisible()
  expect((await page.locator('body').textContent()) ?? '').not.toContain(secret)

  // 2. Confidential connect: /authorize (approve — no consent credential, the secret is the gate)
  //    → code → /token with client_secret + PKCE.
  const prm = await request.get(`${OAUTH_BASE}/.well-known/oauth-protected-resource/mcp`)
  const resource = (await prm.json()).resource as string
  const codeVerifier = b64url(crypto.randomBytes(32))
  const codeChallenge = b64url(crypto.createHash('sha256').update(codeVerifier).digest())
  const redirectURI = 'https://app.example.com/cb'

  // Drive the consent on a throwaway page (the redirect host never resolves — the Stage 2c finding).
  const cb = await page.context().newPage()
  await cb.route(/^https:\/\/app\.example\.com\//, (route) =>
    route.fulfill({ status: 200, contentType: 'text/html', body: '<html>cb</html>' }),
  )
  const authURL =
    `${OAUTH_BASE}/authorize?` +
    new URLSearchParams({
      client_id: clientID,
      redirect_uri: redirectURI,
      response_type: 'code',
      code_challenge: codeChallenge,
      code_challenge_method: 'S256',
      resource,
      state: 'conf-state',
    }).toString()
  await cb.goto(authURL)
  const [redirectReq] = await Promise.all([
    cb.waitForRequest(/app\.example\.com\/cb/),
    cb.click('button[name="approve"]'),
  ])
  const code = new URL(redirectReq.url()).searchParams.get('code')
  expect(code, 'authorization code in the redirect').toBeTruthy()
  await cb.close()

  // Secret-only with no PKCE-able code is meaningless; here we exchange properly (secret + PKCE).
  const tok = await request.post(`${OAUTH_BASE}/token`, {
    form: {
      grant_type: 'authorization_code',
      code: code!,
      client_id: clientID,
      redirect_uri: redirectURI,
      code_verifier: codeVerifier,
      client_secret: secret,
    },
  })
  expect(tok.status(), `confidential token: ${await tok.text()}`).toBe(200)
  const accessToken = (await tok.json()).access_token as string
  expect(accessToken).toBeTruthy()

  // 3. The issued token authorizes a real MCP call (and a missing bearer is 401).
  const unauth = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize())
  expect(unauth.status(), 'no bearer must be 401').toBe(401)
  const authed = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize(accessToken))
  expect(authed.status(), `confidential bearer must authorize (not 401): ${await authed.text()}`).not.toBe(401)

  // 4. Revoke the client through the UI → its row disappears and the token no longer authorizes.
  await page.getByTestId(`client-revoke-${clientID}`).click()
  await page.getByTestId(`client-revoke-confirm-${clientID}`).click()
  await expect(page.getByTestId(`client-row-${clientID}`)).toHaveCount(0)

  const afterRevoke = await request.post(`${OAUTH_BASE}/mcp`, mcpInitialize(accessToken))
  expect(afterRevoke.status(), 'a revoked confidential token must no longer authorize').toBe(401)
})

import { test, expect, type Page, type APIRequestContext } from '@playwright/test'
import crypto from 'node:crypto'

// B-71 Stage 2d — the real-browser, THROUGH-UI proof of DOMAIN-mode OAuth management. The
// operator (single-user admin) drives the OAuth settings screen to:
//   • see the config-seeded trusted domain (example.com) with its consent SET,
//   • CREATE a new trusted domain (with per-domain TTLs),
//   • EDIT its TTL; GENERATE a plaintext consent value (shown always, copyable, re-rollable),
//   • SEE a live connection grouped beneath its domain (and a self-issued token in its own
//     section),
//   • DELETE a domain (its tokens revoked) and watch it disappear.
// Real binary serving the freshly-built bundle over /ws/ui; no page.goto shortcuts into the
// store. The grouping case mints its OWN connection via a real DCR + consent + /token exchange
// (the Stage 2c-proven recipe), so this spec is self-contained — independent of the seeded
// series that connections.spec revokes, and of any other spec's leftover state.

const OAUTH_PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099) - 2
const OAUTH_BASE = `http://localhost:${OAUTH_PORT}`
const CONSENT = 'e2e-consent-secret' // matches global-setup.ts

function b64url(b: Buffer): string {
  return b.toString('base64url')
}

// connectUnderExampleCom mints one live OAuth connection whose client redirects to a subdomain
// of the trusted "example.com", so it groups under that domain card. Returns the client domain
// shown in the row (app.example.com).
//
// The consent flow is driven on a THROWAWAY page: the post-consent redirect targets
// app.example.com, which never resolves (route.fulfill answers the request, but the navigation
// still ends on a chrome-error page — the Stage-2c finding). Isolating it to a page we close
// keeps the caller's main `page` pristine for the subsequent /settings navigation.
async function connectUnderExampleCom(
  page: Page,
  request: APIRequestContext,
): Promise<string> {
  const prm = await request.get(`${OAUTH_BASE}/.well-known/oauth-protected-resource/mcp`)
  expect(prm.ok(), await prm.text()).toBeTruthy()
  const resource = (await prm.json()).resource as string

  const redirectURI = 'https://app.example.com/cb'
  const reg = await request.post(`${OAUTH_BASE}/register`, {
    headers: { 'Content-Type': 'application/json' },
    data: {
      redirect_uris: [redirectURI],
      token_endpoint_auth_method: 'none',
      grant_types: ['authorization_code', 'refresh_token'],
      response_types: ['code'],
      client_name: 'E2E domain-grouping client',
    },
  })
  expect(reg.status(), `register: ${await reg.text()}`).toBe(201)
  const clientID = (await reg.json()).client_id as string

  const codeVerifier = b64url(crypto.randomBytes(32))
  const codeChallenge = b64url(crypto.createHash('sha256').update(codeVerifier).digest())

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
      state: 'grp-state',
    }).toString()
  await cb.goto(authURL)
  await cb.fill('input[name="consent_credential"]', CONSENT)
  const [redirectReq] = await Promise.all([
    cb.waitForRequest(/app\.example\.com\/cb/),
    cb.click('button[name="approve"]'),
  ])
  const code = new URL(redirectReq.url()).searchParams.get('code')
  expect(code, 'authorization code in the redirect').toBeTruthy()
  await cb.close()

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
  return 'app.example.com'
}

// dcrConnect drives a real DCR register → /authorize (submitting `consent`) → /token under
// `redirectHost`, returning the issued access token. Used to prove a GENERATED consent value
// actually authorizes a real connect through the consent page.
async function dcrConnect(
  page: Page,
  request: APIRequestContext,
  redirectHost: string,
  consent: string,
): Promise<string> {
  const prm = await request.get(`${OAUTH_BASE}/.well-known/oauth-protected-resource/mcp`)
  const resource = (await prm.json()).resource as string
  const redirectURI = `https://${redirectHost}/cb`
  const reg = await request.post(`${OAUTH_BASE}/register`, {
    headers: { 'Content-Type': 'application/json' },
    data: {
      redirect_uris: [redirectURI],
      token_endpoint_auth_method: 'none',
      grant_types: ['authorization_code', 'refresh_token'],
      response_types: ['code'],
      client_name: 'E2E generated-consent client',
    },
  })
  expect(reg.status(), `register: ${await reg.text()}`).toBe(201)
  const clientID = (await reg.json()).client_id as string
  const codeVerifier = b64url(crypto.randomBytes(32))
  const codeChallenge = b64url(crypto.createHash('sha256').update(codeVerifier).digest())

  const cb = await page.context().newPage()
  await cb.route(
    (url) => url.href.startsWith(`https://${redirectHost}/`),
    (route) => route.fulfill({ status: 200, contentType: 'text/html', body: '<html>cb</html>' }),
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
      state: 'gen-state',
    }).toString()
  await cb.goto(authURL)
  await cb.fill('input[name="consent_credential"]', consent)
  const [redirectReq] = await Promise.all([
    cb.waitForRequest((req) => req.url().includes(`${redirectHost}/cb`)),
    cb.click('button[name="approve"]'),
  ])
  const code = new URL(redirectReq.url()).searchParams.get('code')
  expect(code, 'authorization code in the redirect').toBeTruthy()
  await cb.close()

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
  return (await tok.json()).access_token as string
}

test.describe.serial('B-71 Stage 2d — domain-mode OAuth management (through the UI)', () => {
  test('the config-seeded domain shows its consent value in plaintext (operator-readable)', async ({
    page,
  }) => {
    await page.goto('/settings?item=oauth')
    await expect(page.getByTestId('domain-row-example.com')).toBeVisible()
    // Consent is plaintext + always visible (2026-06-20): the seeded value is shown so the operator
    // can read/copy it to type at the /authorize callback.
    await expect(page.getByTestId('domain-consent-value-example.com')).toHaveText(CONSENT)
  })

  test('create a trusted domain — it appears with its TTLs and an empty token group', async ({
    page,
  }) => {
    await page.goto('/settings?item=oauth')
    await page.getByTestId('domain-add-identifier').fill('partner.test')
    await page.getByTestId('domain-add-access-ttl').fill('3600')
    await page.getByTestId('domain-add-refresh-ttl').fill('86400')
    await page.getByTestId('domain-add-submit').click()

    const card = page.getByTestId('domain-row-partner.test')
    await expect(card).toBeVisible()
    await expect(card).toContainText('access 1h')
    await expect(card).toContainText('refresh 1d')
    // No consent yet — the card shows a Generate CTA (none ⇒ deny), not a value.
    await expect(page.getByTestId('domain-consent-generate-partner.test')).toBeVisible()
    await expect(page.getByTestId('domain-tokens-partner.test')).toContainText(
      'No active tokens',
    )
  })

  test('reject an empty domain — the submit stays disabled with no identifier', async ({
    page,
  }) => {
    await page.goto('/settings?item=oauth')
    // Pre-submit validation: a TTL with no domain identifier cannot be submitted.
    await page.getByTestId('domain-add-access-ttl').fill('60')
    await expect(page.getByTestId('domain-add-submit')).toBeDisabled()
    await page.getByTestId('domain-add-identifier').fill('ok.test')
    await expect(page.getByTestId('domain-add-submit')).toBeEnabled()
    // A non-finite/negative TTL re-disables it.
    await page.getByTestId('domain-add-access-ttl').fill('-5')
    await expect(page.getByTestId('domain-add-submit')).toBeDisabled()
  })

  test('edit the TTL on the new domain (consent is managed separately)', async ({ page }) => {
    await page.goto('/settings?item=oauth')
    await page.getByTestId('domain-edit-partner.test').click()
    await page.getByTestId('domain-edit-access-ttl-partner.test').fill('7200')
    await page.getByTestId('domain-edit-save-partner.test').click()
    await expect(page.getByTestId('domain-row-partner.test')).toContainText('access 2h')
  })

  test('generate then regenerate a plaintext consent value on the new domain', async ({
    page,
  }) => {
    await page.goto('/settings?item=oauth')
    // Generate mints a value, shown in plaintext (always visible — no reveal panel).
    await page.getByTestId('domain-consent-generate-partner.test').click()
    const value = page.getByTestId('domain-consent-value-partner.test')
    await expect(value).toBeVisible()
    const first = ((await value.textContent()) ?? '').trim()
    expect(first).toBeTruthy()
    // Regenerate re-rolls it (the value changes).
    await page.getByTestId('domain-consent-generate-partner.test').click()
    await expect
      .poll(async () => ((await value.textContent()) ?? '').trim())
      .not.toBe(first)
  })

  test('a generated consent value completes a real connect through the consent page', async ({
    page,
    request,
  }) => {
    // Add a fresh trusted domain and GENERATE its consent through the UI.
    await page.goto('/settings?item=oauth')
    await page.getByTestId('domain-add-identifier').fill('genc.example')
    await page.getByTestId('domain-add-submit').click()
    await expect(page.getByTestId('domain-row-genc.example')).toBeVisible()
    await page.getByTestId('domain-consent-generate-genc.example').click()
    const value = page.getByTestId('domain-consent-value-genc.example')
    await expect(value).toBeVisible()
    const consent = ((await value.textContent()) ?? '').trim()
    expect(consent).toBeTruthy()

    // The SHOWN generated value authorizes a real connect (DCR under app.genc.example).
    const token = await dcrConnect(page, request, 'app.genc.example', consent)
    expect(token, 'the generated consent value must authorize a real connect').toBeTruthy()
  })

  test('a live connection groups beneath its domain; a self-issued token sits apart', async ({
    page,
    request,
  }) => {
    // A DCR client redirecting to app.example.com (a subdomain of the trusted example.com)
    // ⇒ its connection is attributed to the example.com domain entry server-side.
    await connectUnderExampleCom(page, request)

    // Mint an operator self-issued token through the real UI (domain-less).
    await page.goto('/settings?item=oauth')
    await page.getByRole('button', { name: 'Generate a token for the CLI' }).click()
    await expect(page.getByRole('status')).toBeVisible()

    // Reload so both new connections are listed, then assert the grouping SPLIT: a
    // domain-attributed connection groups beneath its domain card, while the
    // domain-less self-issued token renders in its own section. (A connection's
    // client cell shows the client's own id — an opaque DCR handle — so grouping is
    // asserted structurally by where the row lands, which is exactly the Stage 2d
    // behaviour: the server's per-connection `domain` decides the bucket.)
    await page.goto('/settings?item=oauth')
    const grouped = page.getByTestId('domain-tokens-example.com')
    await expect(grouped.locator('[data-testid^="token-row-"]')).not.toHaveCount(0)
    const self = page.getByTestId('self-issued-section')
    await expect(self.locator('[data-testid^="token-row-"]')).not.toHaveCount(0)
  })

  test('delete a domain — its row disappears and the seeded one remains', async ({ page }) => {
    await page.goto('/settings?item=oauth')
    await page.getByTestId('domain-delete-partner.test').click()
    await page.getByTestId('domain-delete-confirm-partner.test').click()
    await expect(page.getByTestId('domain-row-partner.test')).toHaveCount(0)
    await expect(page.getByTestId('domain-row-example.com')).toBeVisible()
  })
})

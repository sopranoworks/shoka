import { test, expect, type Page, type APIRequestContext } from '@playwright/test'
import crypto from 'node:crypto'

const OAUTH_PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099) - 2
const OAUTH_BASE = `http://localhost:${OAUTH_PORT}`
const CONSENT = 'e2e-consent-secret' // matches global-setup.ts
const SCREENSHOT_DIR = '/work/web/tests/e2e/screenshots'

function b64url(b: Buffer): string {
  return b.toString('base64url')
}

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
      client_name: 'E2E scope client',
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
      state: 'scope-state',
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

test.describe.serial('Trusted domain scope configuration', () => {
  test('add a trusted domain with custom scope — no modal', async ({ page }) => {
    await page.goto('/settings?item=oauth')

    await page.getByTestId('domain-add-identifier').fill('scope-test.example')
    await page.getByTestId('domain-add-scope').fill('myns:myproj:r')
    await page.getByTestId('domain-add-access-ttl').fill('3600')
    await page.getByTestId('domain-add-refresh-ttl').fill('86400')
    await page.getByTestId('domain-add-submit').click()

    const card = page.getByTestId('domain-row-scope-test.example')
    await expect(card).toBeVisible()
    await expect(card).toContainText('myns:myproj:r')
    // No confirmation modal should appear for new domain creation.
    await expect(page.getByTestId('scope-change-modal')).toHaveCount(0)

    await page.screenshot({ path: `${SCREENSHOT_DIR}/trusted-domain-scope-input.png`, fullPage: true })
  })

  test('change scope on existing domain — confirmation modal appears', async ({ page, request }) => {
    await page.goto('/settings?item=oauth')

    // Generate consent so we can connect a token to the domain.
    await page.getByTestId('domain-consent-generate-scope-test.example').click()
    const consentEl = page.getByTestId('domain-consent-value-scope-test.example')
    await expect(consentEl).toBeVisible()
    const consent = ((await consentEl.textContent()) ?? '').trim()
    expect(consent).toBeTruthy()

    // Mint a token under the domain so revocation has something to revoke.
    await dcrConnect(page, request, 'app.scope-test.example', consent)

    // Reload to see the token grouped under the domain.
    await page.goto('/settings?item=oauth')
    const tokens = page.getByTestId('domain-tokens-scope-test.example')
    await expect(tokens.locator('[data-testid^="token-row-"]')).not.toHaveCount(0)

    // Edit the domain and change the scope.
    await page.getByTestId('domain-edit-scope-test.example').click()
    const scopeInput = page.getByTestId('domain-edit-scope-scope-test.example')
    await scopeInput.fill('myns:myproj:rw')
    await page.getByTestId('domain-edit-save-scope-test.example').click()

    // The confirmation modal should appear.
    const modal = page.getByTestId('scope-change-modal')
    await expect(modal).toBeVisible()
    await expect(modal).toContainText('Scope Change')
    await expect(modal).toContainText('myns:myproj:r')
    await expect(modal).toContainText('myns:myproj:rw')
    await expect(modal).toContainText('revoke all currently active tokens')

    await page.screenshot({ path: `${SCREENSHOT_DIR}/trusted-domain-scope-change-modal.png`, fullPage: true })

    // Confirm the revocation.
    await page.getByTestId('scope-change-confirm').click()

    // Modal should close and scope should be updated.
    await expect(modal).toHaveCount(0)
    const card = page.getByTestId('domain-row-scope-test.example')
    await expect(card).toContainText('myns:myproj:rw')

    // Tokens should have been revoked — reload to verify.
    await page.goto('/settings?item=oauth')
    const tokensAfter = page.getByTestId('domain-tokens-scope-test.example')
    await expect(tokensAfter).toContainText('No active tokens')

    await page.screenshot({ path: `${SCREENSHOT_DIR}/trusted-domain-scope-updated.png`, fullPage: true })
  })

  test('cancel scope change — scope reverts to original', async ({ page }) => {
    await page.goto('/settings?item=oauth')

    await page.getByTestId('domain-edit-scope-test.example').click()
    const scopeInput = page.getByTestId('domain-edit-scope-scope-test.example')
    await scopeInput.fill('other:scope:r')
    await page.getByTestId('domain-edit-save-scope-test.example').click()

    // Modal should appear.
    const modal = page.getByTestId('scope-change-modal')
    await expect(modal).toBeVisible()

    // Cancel the change.
    await page.getByTestId('scope-change-cancel').click()

    // Modal should close and scope input should revert.
    await expect(modal).toHaveCount(0)
    // The scope on the card should still be the previously saved value.
    const card = page.getByTestId('domain-row-scope-test.example')
    await expect(card).toContainText('myns:myproj:rw')
  })

  test('edit without scope change — no modal, save proceeds directly', async ({ page }) => {
    await page.goto('/settings?item=oauth')

    await page.getByTestId('domain-edit-scope-test.example').click()
    // Only change the TTL, leave scope as-is.
    await page.getByTestId('domain-edit-access-ttl-scope-test.example').fill('7200')
    await page.getByTestId('domain-edit-save-scope-test.example').click()

    // No modal should appear.
    await expect(page.getByTestId('scope-change-modal')).toHaveCount(0)

    // TTL should be updated.
    const card = page.getByTestId('domain-row-scope-test.example')
    await expect(card).toContainText('access 2h')
    // Scope should remain unchanged.
    await expect(card).toContainText('myns:myproj:rw')
  })
})

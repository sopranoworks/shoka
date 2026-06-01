import { test, expect } from '@playwright/test'
import { backendWrite } from './control'

// Session-2 reactivity, against the real binary + /ws/ui NOTIFY stream.
// Fixture (from global-setup): demo/docs (README.md, backlog.md, guides/intro.md,
// config.yaml) and team/handbook (index.md).

test('connection status shows Live when connected', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('Live')).toBeVisible()
})

test('a write to the displayed file banners; content updates only on Reload', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/blob/README.md')
  await expect(page.getByRole('heading', { name: 'Docs' })).toBeVisible()

  await backendWrite('demo', 'docs', 'README.md', '# Docs v2\n\nUpdated body.\n')

  // Banner appears; displayed content is NOT auto-replaced.
  await expect(page.getByText('This file was updated')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Docs', exact: true })).toBeVisible()

  // Reload performs the explicit re-read.
  await page.getByRole('button', { name: 'Reload' }).click()
  await expect(page.getByRole('heading', { name: 'Docs v2' })).toBeVisible()
  await expect(page.getByText('This file was updated')).toBeHidden()
})

test('a write to a non-displayed project shows no banner; tree updates on navigation', async ({
  page,
}) => {
  await page.goto('/')
  await expect(page.getByText('Live')).toBeVisible()

  await backendWrite('team', 'handbook', 'fresh.md', '# Fresh\n')

  // The repository list is not affected by a file write -> no banner.
  await expect(page.getByText(/was updated|files in this project/i)).toHaveCount(0)

  // Navigating to that project shows the new file in the (silently invalidated) tree.
  await page.goto('/p/team/handbook')
  await expect(page.locator('#sidebar').getByText('fresh.md')).toBeVisible()
})

test('reconnects after a dropped WS and surfaces a reconnect banner without losing the write', async ({
  page,
}) => {
  let dropped = false
  let liveWs: import('@playwright/test').WebSocketRoute | null = null

  await page.routeWebSocket(/\/ws\/ui$/, (ws) => {
    if (dropped) {
      ws.close()
      return
    }
    const server = ws.connectToServer()
    liveWs = ws
    ws.onMessage((m) => server.send(m))
    server.onMessage((m) => ws.send(m))
  })

  await page.goto('/p/demo/docs/blob/backlog.md')
  await expect(page.getByText('Live')).toBeVisible()

  // Drop the connection.
  dropped = true
  await liveWs!.close()
  await expect(page.getByText('Live')).toBeHidden()

  // A write happens while the browser is disconnected (its NOTIFY is missed).
  await backendWrite('demo', 'docs', 'backlog.md', '# Backlog v2\n\n- [ ] reconnected\n')

  // Allow reconnection.
  dropped = false
  await expect(page.getByText('Live')).toBeVisible({ timeout: 20000 })

  // Reconnect revalidation surfaces a banner; Reload shows the missed write.
  await expect(page.getByText(/Reconnected/)).toBeVisible()
  await page.getByRole('button', { name: 'Reload' }).click()
  await expect(page.getByRole('heading', { name: 'Backlog v2' })).toBeVisible()
})

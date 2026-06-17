import { test, expect } from '@playwright/test'
import { backendCreateProject, backendWrite } from './control'

// mermaid.js diagram rendering in the markdown viewer, against the REAL binary + production
// bundle in a real Chromium (the trash-D&D bar: a Vitest render test can't prove mermaid
// actually renders SVG in a browser — only this can). Seeds a throwaway project
// demo/diagrams over the shared /ws/ui (super-user pass-through), then drives the real blob
// view. No live data, no live Chrome — a throwaway project the Coder ran headless.

const NS = 'demo'
const PROJ = 'diagrams'
const fence = (lang: string, body: string) => '```' + lang + '\n' + body + '\n```\n'

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'valid.md', '# Valid\n\n' + fence('mermaid', 'graph TD\n  A[Start] --> B[End]'))
  await backendWrite(NS, PROJ, 'broken.md', '# Heading survives\n\n' + fence('mermaid', 'this is @@@ not valid mermaid'))
  await backendWrite(
    NS,
    PROJ,
    'mixed.md',
    '# Mixed\n\n' + fence('go', 'func main() { println("hi") }') + '\n' + fence('mermaid', 'sequenceDiagram\n  Alice->>Bob: Hi'),
  )
  await backendWrite(NS, PROJ, 'second.md', '# Second doc\n\n' + fence('mermaid', 'graph LR\n  X --> Y'))
})

// #1 (core) — a valid mermaid fence renders an actual SVG diagram, NOT the raw code.
test('renders a valid mermaid block as an SVG diagram', async ({ page }) => {
  await page.goto(`/p/${NS}/${PROJ}/blob/valid.md`)
  const diagram = page.getByTestId('mermaid-diagram')
  await expect(diagram).toBeVisible({ timeout: 15000 })
  await expect(diagram.locator('svg')).toBeVisible()
  // The mermaid SOURCE is consumed into the diagram, not shown as a raw code fence.
  await expect(page.getByText('graph TD', { exact: false })).toHaveCount(0)
})

// #2 (core) — a malformed mermaid block shows an inline error + raw fallback, and the rest of
// the document still renders. The viewer does not crash/blank.
test('a malformed mermaid block degrades gracefully', async ({ page }) => {
  await page.goto(`/p/${NS}/${PROJ}/blob/broken.md`)
  await expect(page.getByTestId('mermaid-error')).toBeVisible({ timeout: 15000 })
  // The rest of the document rendered.
  await expect(page.getByRole('heading', { name: 'Heading survives' })).toBeVisible()
  // The raw source is shown as the fallback.
  await expect(page.getByTestId('mermaid-error')).toContainText('not valid mermaid')
})

// #3 — a non-mermaid code fence is unchanged: the go fence highlights AND the mermaid fence
// renders a diagram, side by side.
test('non-mermaid code fences are unchanged alongside a diagram', async ({ page }) => {
  await page.goto(`/p/${NS}/${PROJ}/blob/mixed.md`)
  await expect(page.getByTestId('mermaid-diagram').locator('svg')).toBeVisible({ timeout: 15000 })
  // The go fence is a normal highlighted code block (rehype-highlight .hljs), not a diagram.
  const hljs = page.locator('pre code.hljs, pre code.language-go')
  await expect(hljs.first()).toBeVisible()
  await expect(hljs.first()).toContainText('func main')
})

// #4 — navigating to a different document re-renders the new diagram (no stale/double render).
test('re-renders the diagram when navigating to another document', async ({ page }) => {
  await page.goto(`/p/${NS}/${PROJ}/blob/valid.md`)
  await expect(page.getByTestId('mermaid-diagram').locator('svg')).toBeVisible({ timeout: 15000 })
  await page.goto(`/p/${NS}/${PROJ}/blob/second.md`)
  await expect(page.getByRole('heading', { name: 'Second doc' })).toBeVisible()
  // Exactly one diagram, freshly rendered for the new content.
  await expect(page.getByTestId('mermaid-diagram').locator('svg')).toBeVisible({ timeout: 15000 })
  await expect(page.getByTestId('mermaid-diagram')).toHaveCount(1)
})

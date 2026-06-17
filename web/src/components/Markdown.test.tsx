import { render, screen, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'

// mermaid is dynamically imported by MermaidDiagram; mock it so the Vitest (jsdom) suite can
// drive the fence-routing + graceful-failure LOGIC deterministically. The REAL browser render
// (an actual <svg>) is proven by the Playwright E2E (the trash-D&D bar) — jsdom cannot render
// mermaid's SVG, so a Vitest-only assertion would not prove the library works.
const { parse, mermaidRender, initialize } = vi.hoisted(() => ({
  parse: vi.fn(),
  mermaidRender: vi.fn(),
  initialize: vi.fn(),
}))
vi.mock('mermaid', () => ({ default: { initialize, parse, render: mermaidRender } }))

import { Markdown } from './Markdown'

const fence = (lang: string, body: string) => '```' + lang + '\n' + body + '\n```\n'

describe('Markdown mermaid integration', () => {
  beforeEach(() => {
    parse.mockReset()
    mermaidRender.mockReset()
    initialize.mockReset()
  })

  // #1 — a valid ```mermaid fence routes to the diagram renderer (not raw code).
  it('renders a valid mermaid fence as a diagram, not raw code', async () => {
    parse.mockResolvedValue(true)
    mermaidRender.mockResolvedValue({ svg: '<svg role="img" aria-label="diagram"></svg>' })
    render(<Markdown content={'# Title\n\n' + fence('mermaid', 'graph TD; A-->B;')} />)
    await waitFor(() => expect(screen.getByTestId('mermaid-diagram')).toBeInTheDocument())
    // The fence text is not shown as a code block.
    expect(screen.queryByText('graph TD; A-->B;')).toBeNull()
    // mermaid was initialized once with the explicit-render config.
    expect(initialize).toHaveBeenCalledWith(expect.objectContaining({ startOnLoad: false }))
  })

  // #2 (core) — a malformed mermaid fence shows an inline error + raw fallback, and the REST of
  // the document still renders. The viewer does not crash/blank.
  it('shows an inline error + raw fallback for a malformed mermaid block, rest still renders', async () => {
    parse.mockResolvedValue(false) // invalid syntax
    render(<Markdown content={'# Heading stays\n\n' + fence('mermaid', 'not a diagram !!!')} />)
    await waitFor(() => expect(screen.getByTestId('mermaid-error')).toBeInTheDocument())
    // Raw source is shown as the fallback.
    expect(screen.getByTestId('mermaid-error')).toHaveTextContent('not a diagram !!!')
    // The rest of the document rendered normally.
    expect(screen.getByRole('heading', { name: 'Heading stays' })).toBeInTheDocument()
    // render() was never reached (parse rejected it first).
    expect(mermaidRender).not.toHaveBeenCalled()
  })

  // #2b — a render-time throw (valid parse, render rejects) also degrades gracefully.
  it('degrades gracefully when mermaid.render throws', async () => {
    parse.mockResolvedValue(true)
    mermaidRender.mockRejectedValue(new Error('boom'))
    render(<Markdown content={fence('mermaid', 'graph TD; A-->B;')} />)
    await waitFor(() => expect(screen.getByTestId('mermaid-error')).toBeInTheDocument())
  })

  // #3 — a non-mermaid code fence is unchanged (rehype-highlight code block, no diagram).
  it('leaves a normal code fence unchanged', () => {
    const { container } = render(<Markdown content={fence('go', 'func main() {}')} />)
    expect(screen.queryByTestId('mermaid-diagram')).toBeNull()
    expect(screen.queryByTestId('mermaid-error')).toBeNull()
    // The code renders inside a highlighted <pre><code class="language-go hljs"> (rehype-highlight
    // splits tokens into spans, so read the concatenated textContent).
    const code = container.querySelector('pre code')
    expect(code).not.toBeNull()
    expect(code?.className).toContain('language-go')
    expect(code?.textContent).toContain('func main() {}')
    expect(parse).not.toHaveBeenCalled()
  })

  // #4 — changing the content re-renders the new diagram (the file-navigation path).
  it('re-renders the diagram when content changes', async () => {
    parse.mockResolvedValue(true)
    mermaidRender.mockResolvedValue({ svg: '<svg></svg>' })
    const { rerender } = render(<Markdown content={fence('mermaid', 'graph TD; A-->B;')} />)
    await waitFor(() => expect(screen.getByTestId('mermaid-diagram')).toBeInTheDocument())
    mermaidRender.mockClear()
    rerender(<Markdown content={fence('mermaid', 'graph LR; X-->Y;')} />)
    await waitFor(() => expect(mermaidRender).toHaveBeenCalledWith(expect.any(String), expect.stringContaining('graph LR; X-->Y;')))
  })
})

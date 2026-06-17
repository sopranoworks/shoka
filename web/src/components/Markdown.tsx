import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import type { Element } from 'hast'
import { MermaidDiagram } from './MermaidDiagram'
import styles from './Markdown.module.css'
import './highlight.css'

// Markdown viewer: headings, lists, code blocks, links, GFM tables.
//
// Fenced code blocks are syntax-highlighted by rehype-highlight (session 4),
// which operates on the rehype AST and emits .hljs-* classes — no
// dangerouslySetInnerHTML. It statically bundles highlight.js's "common"
// language set (~54 KB gz), which covers the corpus fences (go/yaml/json/ts/
// sql/sh, per the data-dir survey) and more; passing a curated `languages`
// option would only narrow runtime registration, not the bundle (the import is
// static), so it would reduce capability without saving size. The token colors
// live in ./highlight.css (theme-aware); the block container is styled by
// Markdown.module.css. detect:false → only labelled fences highlight (plain
// prose is left alone); ignoreMissing → an unknown language is left unhighlighted
// rather than throwing.
const rehypePlugins = [
  [rehypeHighlight, { detect: false, ignoreMissing: true }],
] as const

// mermaidSource returns the raw fence text when a <pre>'s child is a ```mermaid code block,
// else null. rehype-highlight leaves an unknown language (mermaid, via ignoreMissing:true)
// untouched, so the code node still holds a single raw text child — read it directly from the
// hast node. Detecting at the <pre> level (not <code>) lets us replace the whole block with the
// diagram, avoiding an invalid block element nested inside <pre>.
function mermaidSource(node: Element | undefined): string | null {
  const code = node?.children?.find((c): c is Element => c.type === 'element' && c.tagName === 'code')
  if (!code) return null
  const cls = code.properties?.className
  const isMermaid = Array.isArray(cls) && cls.includes('language-mermaid')
  if (!isMermaid) return null
  const text = code.children
    .map((c) => (c.type === 'text' ? c.value : ''))
    .join('')
  return text.replace(/\n$/, '')
}

// The viewer renders ```mermaid fences as diagrams (MermaidDiagram); every other code fence
// and all other markdown render exactly as before (rehype-highlight on the <code>, default
// <pre> wrapper).
const components: Components = {
  pre(props) {
    const { node, children, ...rest } = props
    const src = mermaidSource(node)
    if (src !== null) {
      return <MermaidDiagram code={src} />
    }
    return <pre {...rest}>{children}</pre>
  },
}

export function Markdown({ content }: { content: string }) {
  return (
    <div className={styles.md}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={rehypePlugins as never}
        components={components}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
}

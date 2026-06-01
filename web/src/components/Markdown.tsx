import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
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

export function Markdown({ content }: { content: string }) {
  return (
    <div className={styles.md}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={rehypePlugins as never}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
}

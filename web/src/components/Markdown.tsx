import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import styles from './Markdown.module.css'
import './highlight.css'

// Markdown viewer: headings, lists, code blocks, links, GFM tables.
//
// Fenced code blocks are syntax-highlighted by rehype-highlight (session 4),
// which operates on the rehype AST and emits .hljs-* classes — no
// dangerouslySetInnerHTML. ignoreMissing keeps an unknown fence language from
// throwing; detect:false means only labelled fences highlight (plain text is
// left alone). The token colors live in ./highlight.css (theme-aware); the
// block container is styled by Markdown.module.css.
const rehypePlugins = [[rehypeHighlight, { detect: false, ignoreMissing: true }]] as const

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

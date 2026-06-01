import type { Extension } from '@codemirror/state'
import { markdown } from '@codemirror/lang-markdown'
import { json } from '@codemirror/lang-json'
import { yaml } from '@codemirror/lang-yaml'
import { javascript } from '@codemirror/lang-javascript'
import { go } from '@codemirror/lang-go'

// Map a file path's extension to a CodeMirror language extension (session 4).
// Curated to the languages the Shoka corpus actually produces — markdown
// dominates; Go/YAML/JSON/TS/JS appear (mostly inside markdown fences, possibly
// as standalone files). Unknown extensions get no language (plain text), which
// is correct for the .txt files that make up the rest of the corpus.
//
// These @codemirror/lang-* packages are imported only here, and this module is
// pulled in only by the lazy editor/code-view chunk, so they never weigh on the
// initial bundle (see the route-level code-split).
export function languageForPath(path: string): Extension[] {
  const dot = path.lastIndexOf('.')
  const ext = dot >= 0 ? path.slice(dot + 1).toLowerCase() : ''
  switch (ext) {
    case 'md':
    case 'markdown':
      return [markdown()]
    case 'json':
      return [json()]
    case 'yaml':
    case 'yml':
      return [yaml()]
    case 'js':
    case 'jsx':
    case 'mjs':
    case 'cjs':
      return [javascript()]
    case 'ts':
    case 'tsx':
      return [javascript({ typescript: true })]
    case 'go':
      return [go()]
    default:
      return []
  }
}

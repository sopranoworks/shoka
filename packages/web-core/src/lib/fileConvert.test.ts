import { describe, it, expect } from 'vitest'
import {
  needsConversion,
  convertedName,
  validateUtf8,
  csvToMarkdownTable,
  prepareConversions,
  convertCandidate,
} from './fileConvert'

describe('needsConversion', () => {
  it('returns .csv for CSV files', () => {
    expect(needsConversion('data.csv')).toBe('.csv')
    expect(needsConversion('data.CSV')).toBe('.csv')
    expect(needsConversion('path/to/data.csv')).toBe('.csv')
  })

  it('returns .txt for TXT files', () => {
    expect(needsConversion('notes.txt')).toBe('.txt')
    expect(needsConversion('notes.TXT')).toBe('.txt')
  })

  it('returns null for other extensions', () => {
    expect(needsConversion('doc.md')).toBeNull()
    expect(needsConversion('doc.json')).toBeNull()
    expect(needsConversion('image.png')).toBeNull()
    expect(needsConversion('Makefile')).toBeNull()
  })
})

describe('convertedName', () => {
  it('replaces .csv with .md', () => {
    expect(convertedName('data.csv')).toBe('data.md')
  })

  it('replaces .txt with .md', () => {
    expect(convertedName('notes.txt')).toBe('notes.md')
  })

  it('handles multiple dots', () => {
    expect(convertedName('data.2024.csv')).toBe('data.2024.md')
  })

  it('handles extensionless names', () => {
    expect(convertedName('Makefile')).toBe('Makefile.md')
  })
})

describe('validateUtf8', () => {
  it('decodes valid UTF-8', () => {
    const bytes = new TextEncoder().encode('hello world')
    expect(validateUtf8(bytes)).toBe('hello world')
  })

  it('decodes Japanese UTF-8', () => {
    const bytes = new TextEncoder().encode('こんにちは')
    expect(validateUtf8(bytes)).toBe('こんにちは')
  })

  it('strips BOM and returns content', () => {
    const bom = new Uint8Array([0xef, 0xbb, 0xbf])
    const content = new TextEncoder().encode('hello')
    const withBom = new Uint8Array([...bom, ...content])
    expect(validateUtf8(withBom)).toBe('hello')
  })

  it('throws on invalid UTF-8 bytes', () => {
    const bad = new Uint8Array([0xff, 0xfe, 0x00])
    expect(() => validateUtf8(bad)).toThrow()
  })

  it('throws on truncated multi-byte sequence', () => {
    const bad = new Uint8Array([0xc3]) // start of 2-byte sequence, missing continuation
    expect(() => validateUtf8(bad)).toThrow()
  })
})

describe('csvToMarkdownTable', () => {
  it('converts a basic CSV to a Markdown table', () => {
    const csv = 'Name,Age,City\nAlice,30,Tokyo\nBob,25,Osaka\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toBe(
      '| Name | Age | City |\n' +
      '| --- | --- | --- |\n' +
      '| Alice | 30 | Tokyo |\n' +
      '| Bob | 25 | Osaka |\n',
    )
  })

  it('escapes pipe characters in cell values', () => {
    const csv = 'Col1,Col2\na|b,c\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toContain('a\\|b')
  })

  it('preserves empty cells', () => {
    const csv = 'A,B,C\n1,,3\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toBe(
      '| A | B | C |\n' +
      '| --- | --- | --- |\n' +
      '| 1 |  | 3 |\n',
    )
  })

  it('handles quoted fields with commas', () => {
    const csv = 'Name,Desc\nAlice,"hello, world"\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toContain('| Alice | hello, world |')
  })

  it('handles quoted fields with escaped quotes', () => {
    const csv = 'A,B\n"say ""hi""",val\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toContain('say "hi"')
  })

  it('handles CSV without trailing newline', () => {
    const csv = 'A,B\n1,2'
    const md = csvToMarkdownTable(csv)
    expect(md).toBe('| A | B |\n| --- | --- |\n| 1 | 2 |\n')
  })

  it('handles header-only CSV', () => {
    const csv = 'A,B,C\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toBe('| A | B | C |\n| --- | --- | --- |\n')
  })

  it('pads short rows to match header count', () => {
    const csv = 'A,B,C\n1\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toBe('| A | B | C |\n| --- | --- | --- |\n| 1 |  |  |\n')
  })

  it('returns empty string for empty input', () => {
    expect(csvToMarkdownTable('')).toBe('')
  })

  it('handles CRLF line endings', () => {
    const csv = 'A,B\r\n1,2\r\n'
    const md = csvToMarkdownTable(csv)
    expect(md).toBe('| A | B |\n| --- | --- |\n| 1 | 2 |\n')
  })
})

describe('prepareConversions', () => {
  it('separates convertible files and validates UTF-8', async () => {
    const csvFile = new File(['A,B\n1,2\n'], 'data.csv', { type: 'text/csv' })
    const mdFile = new File(['# hello'], 'readme.md', { type: 'text/markdown' })
    const { candidates, errors } = await prepareConversions([csvFile, mdFile])
    expect(candidates).toHaveLength(1)
    expect(candidates[0].originalName).toBe('data.csv')
    expect(candidates[0].convertedName).toBe('data.md')
    expect(candidates[0].type).toBe('.csv')
    expect(errors).toHaveLength(0)
  })

  it('reports errors for non-UTF-8 files', async () => {
    const bad = new File([new Uint8Array([0xff, 0xfe, 0x00])], 'bad.csv', {
      type: 'text/csv',
    })
    const { candidates, errors } = await prepareConversions([bad])
    expect(candidates).toHaveLength(0)
    expect(errors).toHaveLength(1)
    expect(errors[0]).toContain('UTF-8')
  })
})

describe('convertCandidate', () => {
  it('converts a CSV candidate to a markdown File', () => {
    const file = convertCandidate({
      originalName: 'data.csv',
      convertedName: 'data.md',
      type: '.csv',
      text: 'A,B\n1,2\n',
    })
    expect(file.name).toBe('data.md')
  })

  it('converts a TXT candidate without transforming content', () => {
    const file = convertCandidate({
      originalName: 'notes.txt',
      convertedName: 'notes.md',
      type: '.txt',
      text: 'plain text content',
    })
    expect(file.name).toBe('notes.md')
    expect(file.size).toBe('plain text content'.length)
  })
})

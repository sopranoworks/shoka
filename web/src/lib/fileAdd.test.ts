import { describe, it, expect, vi, beforeEach } from 'vitest'

const saveFile = vi.fn()
vi.mock('./fileOps', () => ({
  saveFile: (args: unknown) => saveFile(args),
}))

import {
  extOf,
  isAllowedAddFile,
  joinAddPath,
  fileToBase64,
  addDroppedFile,
} from './fileAdd'

describe('fileAdd path/format helpers', () => {
  it('extOf mirrors filepath.Ext (last dot of basename, lowercased)', () => {
    expect(extOf('a.md')).toBe('.md')
    expect(extOf('sub/dir/b.YAML')).toBe('.yaml')
    expect(extOf('archive.tar.gz')).toBe('.gz')
    expect(extOf('Makefile')).toBe('')
    expect(extOf('LICENSE')).toBe('')
  })

  it('isAllowedAddFile accepts only the closed markdown/json/yaml set', () => {
    for (const ok of ['x.md', 'x.markdown', 'x.json', 'x.yaml', 'x.yml', 'X.MD'])
      expect(isAllowedAddFile(ok)).toBe(true)
    for (const no of ['x.png', 'x.pdf', 'x.txt', 'Makefile', 'x.tar.gz'])
      expect(isAllowedAddFile(no)).toBe(false)
  })

  it('joinAddPath places the file under the dest dir, or at root when empty', () => {
    expect(joinAddPath('', 'a.md')).toBe('a.md')
    expect(joinAddPath('docs', 'a.md')).toBe('docs/a.md')
    expect(joinAddPath('docs/notes/', 'a.md')).toBe('docs/notes/a.md')
  })

  it('fileToBase64 returns the raw bytes as base64 (no data: prefix)', async () => {
    const bytes = new Uint8Array([0xff, 0xfe, 0x00, 0x41])
    const b64 = await fileToBase64(new Blob([bytes]))
    expect(b64).toBe('//4AQQ==') // base64 of ff fe 00 41
  })
})

describe('addDroppedFile', () => {
  beforeEach(() => saveFile.mockReset())

  const file = (name: string, content = '# x\n') =>
    new File([content], name, { type: 'text/markdown' })

  it('rejects a non-allowlisted file WITHOUT sending a write', async () => {
    const out = await addDroppedFile({
      namespace: 'n',
      project: 'p',
      destDir: '',
      file: file('image.png'),
      confirmOverwrite: () => true,
    })
    expect(out.status).toBe('rejected')
    expect(out.message).toMatch(/only .* files can be added/i)
    expect(saveFile).not.toHaveBeenCalled()
  })

  it('adds an allowlisted file at the resolved path via the base64 path', async () => {
    saveFile.mockResolvedValue({ ok: true, path: 'docs/a.md', etag: 'e1' })
    const out = await addDroppedFile({
      namespace: 'n',
      project: 'p',
      destDir: 'docs',
      file: file('a.md'),
      confirmOverwrite: () => true,
    })
    expect(out.status).toBe('added')
    expect(out.path).toBe('docs/a.md')
    expect(saveFile).toHaveBeenCalledTimes(1)
    expect(saveFile.mock.calls[0][0]).toMatchObject({
      namespace: 'n',
      project: 'p',
      path: 'docs/a.md',
      contentEncoding: 'base64',
    })
    // No if_match on the first (create) attempt.
    expect('ifMatch' in saveFile.mock.calls[0][0]).toBe(false)
  })

  it('on a collision, confirming overwrites with the current etag as if_match', async () => {
    saveFile
      .mockResolvedValueOnce({ ok: false, path: 'a.md', currentEtag: 'eCur', message: 'exists' })
      .mockResolvedValueOnce({ ok: true, path: 'a.md', etag: 'eNew' })
    const out = await addDroppedFile({
      namespace: 'n',
      project: 'p',
      destDir: '',
      file: file('a.md'),
      confirmOverwrite: () => true,
    })
    expect(out.status).toBe('overwritten')
    expect(saveFile).toHaveBeenCalledTimes(2)
    expect(saveFile.mock.calls[1][0]).toMatchObject({ ifMatch: 'eCur', contentEncoding: 'base64' })
  })

  it('on a collision, declining leaves the existing file untouched (skipped)', async () => {
    saveFile.mockResolvedValueOnce({ ok: false, path: 'a.md', currentEtag: 'eCur', message: 'exists' })
    const out = await addDroppedFile({
      namespace: 'n',
      project: 'p',
      destDir: '',
      file: file('a.md'),
      confirmOverwrite: () => false,
    })
    expect(out.status).toBe('skipped')
    expect(saveFile).toHaveBeenCalledTimes(1) // never resent
  })

  it('never throws: a saveFile rejection becomes an error outcome', async () => {
    // Create the rejection at call-time (not eagerly) so its handler is attached
    // on the same tick addDroppedFile awaits it — otherwise the await of
    // fileToBase64 first would let Node flag the pre-created rejection as unhandled.
    saveFile.mockImplementationOnce(() => Promise.reject(new Error('boom')))
    const out = await addDroppedFile({
      namespace: 'n',
      project: 'p',
      destDir: '',
      file: file('a.md'),
      confirmOverwrite: () => true,
    })
    expect(out.status).toBe('error')
    expect(out.message).toBe('boom')
  })
})

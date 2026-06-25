import { describe, it, expect } from 'vitest'
import { fileOpenRoute } from '@shoka/web-core'

// In the sidebar's History mode the tree opens each file's history (so the right
// pane follows the selection); otherwise it opens the file view. (B-31 fix A.)
describe('fileOpenRoute', () => {
  it('opens the file view in the default (blob) mode', () => {
    expect(fileOpenRoute('blob')).toBe('/p/$namespace/$project/blob/$')
  })

  it('opens the file’s history in history mode', () => {
    expect(fileOpenRoute('history')).toBe('/p/$namespace/$project/history/$')
  })
})

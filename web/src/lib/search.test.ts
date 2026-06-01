import { describe, it, expect, vi, beforeEach } from 'vitest'

const request = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ request }),
}))

import { searchFiles } from './search'

describe('searchFiles', () => {
  beforeEach(() => {
    request.mockReset()
  })

  it('sends a project-scoped SEARCH_FILES request and returns matches', async () => {
    request.mockResolvedValue({
      matches: [
        { path: 'a.md', snippet: '…hit…' },
        { path: 'dir/b.md' },
      ],
    })
    const matches = await searchFiles('ns', 'proj', 'query')
    expect(request).toHaveBeenCalledWith('SEARCH_FILES', {
      namespace: 'ns',
      projectName: 'proj',
      query: 'query',
      search_in: 'both',
    })
    expect(matches).toEqual([
      { path: 'a.md', snippet: '…hit…' },
      { path: 'dir/b.md' },
    ])
  })

  it('normalises a missing matches field to an empty array', async () => {
    request.mockResolvedValue({})
    const matches = await searchFiles('ns', 'proj', 'q')
    expect(matches).toEqual([])
  })
})

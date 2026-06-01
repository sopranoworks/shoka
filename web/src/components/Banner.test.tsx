import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { BannerProvider, useBanner } from '../lib/banner'
import { Banner } from './Banner'

function Shower({ reload }: { reload: () => void }) {
  const { show } = useBanner()
  return (
    <button onClick={() => show({ text: 'This file was updated', reload })}>
      trigger
    </button>
  )
}

describe('Banner', () => {
  it('hidden until shown; Reload calls reload then clears; Dismiss clears', async () => {
    const user = userEvent.setup()
    const reload = vi.fn()
    render(
      <BannerProvider>
        <Banner />
        <Shower reload={reload} />
      </BannerProvider>,
    )

    expect(screen.queryByText('This file was updated')).toBeNull()

    await user.click(screen.getByText('trigger'))
    expect(screen.getByText('This file was updated')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Reload' }))
    expect(reload).toHaveBeenCalledOnce()
    expect(screen.queryByText('This file was updated')).toBeNull()

    // Show again, then Dismiss hides without reloading.
    await user.click(screen.getByText('trigger'))
    await user.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(reload).toHaveBeenCalledOnce()
    expect(screen.queryByText('This file was updated')).toBeNull()
  })

  it('collapses multiple shows into one banner', async () => {
    const user = userEvent.setup()
    render(
      <BannerProvider>
        <Banner />
        <Shower reload={() => {}} />
      </BannerProvider>,
    )
    await user.click(screen.getByText('trigger'))
    await user.click(screen.getByText('trigger'))
    expect(screen.getAllByText('This file was updated')).toHaveLength(1)
  })
})

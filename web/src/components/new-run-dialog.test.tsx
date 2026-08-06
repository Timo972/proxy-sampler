import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { NewRunDialog } from './new-run-dialog'

function renderDialog() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={client}>
      <NewRunDialog open onClose={() => {}} />
    </QueryClientProvider>,
  )
}

describe('NewRunDialog', () => {
  it('shows a live variant-count preview and blocks over the cap', async () => {
    renderDialog()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Run name'), 'poolcheck')
    await user.type(screen.getByLabelText('Proxy template'), 'socks5h://u-cc-{country}:pw@gate:1080')
    // Add a list axis "country" with 4 values.
    await user.click(screen.getByRole('button', { name: 'Add axis' }))
    await user.type(screen.getByLabelText('Axis 1 name'), 'country')
    await user.selectOptions(screen.getByLabelText('Axis 1 kind'), 'list')
    await user.type(screen.getByLabelText('Axis 1 values'), 'de, us, fr, jp')
    expect(await screen.findByText(/4 variants/)).toBeInTheDocument()
  })

  it('requires a name, template, and at least one axis', async () => {
    vi.stubGlobal('fetch', vi.fn())
    renderDialog()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Start run' }))
    expect(await screen.findByText('Name is required')).toBeInTheDocument()
    expect(fetch).not.toHaveBeenCalled()
  })
})

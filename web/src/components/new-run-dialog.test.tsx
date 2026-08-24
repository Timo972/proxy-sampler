import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { NewRunDialog } from './new-run-dialog'

function stubFetch(config = 128) {
  const fn = vi.fn((input: RequestInfo | URL) => {
    const url = String(input)
    const body = url.endsWith('/api/config') ? { max_variants_per_run: config } : {}
    return Promise.resolve(
      new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    )
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

function callsTo(fetchMock: ReturnType<typeof vi.fn>, suffix: string) {
  return fetchMock.mock.calls.filter(([input]) => String(input).endsWith(suffix))
}

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
    stubFetch()
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
    const fetchMock = stubFetch()
    renderDialog()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Start run' }))
    expect(await screen.findByText('Name is required')).toBeInTheDocument()
    // The run is never submitted (config may be fetched, but /api/runs is not).
    expect(callsTo(fetchMock, '/api/runs')).toHaveLength(0)
  })

  it('rejects duplicate axis names', async () => {
    stubFetch()
    renderDialog()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Run name'), 'dup')
    await user.type(screen.getByLabelText('Proxy template'), 'socks5h://u-cc-{country}:pw@gate:1080')
    await user.click(screen.getByRole('button', { name: 'Add axis' }))
    await user.click(screen.getByRole('button', { name: 'Add axis' }))
    await user.type(screen.getByLabelText('Axis 1 name'), 'country')
    await user.type(screen.getByLabelText('Axis 2 name'), 'country')
    await user.click(screen.getByRole('button', { name: 'Start run' }))
    expect(await screen.findByText('Axis names must be unique')).toBeInTheDocument()
  })

  it('submits the manual target country with the run', async () => {
    const fetchMock = stubFetch()
    renderDialog()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Run name'), 'geo')
    await user.type(screen.getByLabelText('Proxy template'), 'socks5h://u-sid-{session}:pw@gate:1080')
    await user.click(screen.getByRole('button', { name: 'Add axis' }))
    await user.type(screen.getByLabelText('Axis 1 name'), 'session')
    await user.selectOptions(screen.getByLabelText('Axis 1 kind'), 'random')
    await user.type(screen.getByLabelText('Axis 1 count'), '2')
    await user.selectOptions(screen.getByLabelText('Sampling mode'), 'sticky')
    await user.click(screen.getByRole('button', { name: 'Advanced settings' }))
    await user.type(screen.getByLabelText('Target country (optional)'), 'us')
    await user.click(screen.getByRole('button', { name: 'Start run' }))
    await vi.waitFor(() => expect(callsTo(fetchMock, '/api/runs')).toHaveLength(1))
    const body = JSON.parse((callsTo(fetchMock, '/api/runs')[0][1] as RequestInit).body as string)
    expect(body.target_country).toBe('us')
  })

  it('caps the preview using the server-configured maximum', async () => {
    stubFetch(3)
    renderDialog()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Proxy template'), 'socks5h://u-cc-{country}:pw@gate:1080')
    await user.click(screen.getByRole('button', { name: 'Add axis' }))
    await user.type(screen.getByLabelText('Axis 1 name'), 'country')
    await user.selectOptions(screen.getByLabelText('Axis 1 kind'), 'list')
    await user.type(screen.getByLabelText('Axis 1 values'), 'de, us, fr, jp')
    // 4 variants exceeds the server cap of 3, so the over-cap message uses it.
    expect(await screen.findByText(/exceeds the maximum of 3/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Start run' })).toBeDisabled()
  })
})

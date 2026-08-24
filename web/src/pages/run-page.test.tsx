import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'

import { RunPage } from './run-page'

function mockFetch(detail: unknown, report: unknown) {
  return vi.fn((input: RequestInfo | URL) => {
    const url = String(input)
    const body = url.endsWith('/report') ? report : detail
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }))
  })
}

describe('RunPage', () => {
  it('renders the pool rollup stat strip', async () => {
    const detail = { run: { id: 'r1', name: 'poolcheck', template_display: 'gate:1080', status: 'running', variant_count: 3, distinct_ips: 12, created_at: new Date().toISOString() }, variants: [] }
    const report = { distinct_ips: 12, estimated_pool_size: 40, pool_size_lower_bound: false, honor_rate: 1, composition: { mobile: 0, residential: 12, datacenter: 0, unknown: 0 }, risk_histogram: [], flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0, series: [], cells: [], ips: [] }
    vi.stubGlobal('fetch', mockFetch(detail, report))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/runs/r1']}>
          <Routes><Route path="/runs/:id" element={<RunPage />} /></Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    await waitFor(() => expect(screen.getByText('poolcheck')).toBeInTheDocument())
    expect(await screen.findByText(/40/)).toBeInTheDocument() // estimated pool size
  })

  it('shows the manual target country the honor rate is measured against', async () => {
    const detail = {
      run: { id: 'r1', name: 'geo', template_display: 'gate:1080', status: 'running', variant_count: 1, distinct_ips: 0, created_at: new Date().toISOString() },
      variants: [{ session_id: 's1', name: 'geo #1', cell_key: '{}', params: { session: 'ab12' }, target_country: 'US', status: 'running', samples_taken: 0, distinct_ips: 0 }],
    }
    const report = { distinct_ips: 0, estimated_pool_size: 0, pool_size_lower_bound: false, honor_rate: null, composition: { mobile: 0, residential: 0, datacenter: 0, unknown: 0 }, risk_histogram: [], flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0, series: [], cells: [], ips: [] }
    vi.stubGlobal('fetch', mockFetch(detail, report))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/runs/r1']}>
          <Routes><Route path="/runs/:id" element={<RunPage />} /></Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    await waitFor(() => expect(screen.getByText('geo')).toBeInTheDocument())
    expect(screen.getByText(/target country/i)).toBeInTheDocument()
    expect(screen.getByText('US')).toBeInTheDocument()
  })

  it('labels a cell with the manual target its honor rate was measured against', async () => {
    const detail = {
      run: { id: 'r1', name: 'geo', template_display: 'gate:1080', status: 'running', variant_count: 1, distinct_ips: 1, created_at: new Date().toISOString() },
      variants: [{ session_id: 's1', name: 'geo #1', cell_key: '{"country":"de"}', params: { country: 'de' }, target_country: 'US', status: 'running', samples_taken: 2, distinct_ips: 1 }],
    }
    const report = {
      distinct_ips: 1, estimated_pool_size: 1, pool_size_lower_bound: false, honor_rate: 1,
      composition: { mobile: 0, residential: 1, datacenter: 0, unknown: 0 }, risk_histogram: [],
      flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0, series: [],
      cells: [{ cell_key: '{"country":"de"}', params: { country: 'de' }, target_country: 'US', variant_count: 1, distinct_ips: 1, honor_rate: 1, composition: { mobile: 0, residential: 1, datacenter: 0, unknown: 0 } }],
      ips: [],
    }
    vi.stubGlobal('fetch', mockFetch(detail, report))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/runs/r1']}>
          <Routes><Route path="/runs/:id" element={<RunPage />} /></Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    await waitFor(() => expect(screen.getByText('geo')).toBeInTheDocument())
    // The cell label must disclose the override: params say country=de, but the
    // honor rate was measured against the manual target US. country=de appears
    // in both the cell and variants tables, so assert presence, not uniqueness.
    expect((await screen.findAllByText(/country=de/)).length).toBeGreaterThan(0)
    expect(screen.getByText(/target US \(manual\)/)).toBeInTheDocument()
  })

  it('renames the run from the header and shows the cascaded variant name', async () => {
    let name = 'Alpha'
    const detail = () => ({
      run: { id: 'r1', name, template_display: 'gate:1080', status: 'running', variant_count: 1, distinct_ips: 0, created_at: new Date().toISOString() },
      variants: [{ session_id: 's1', name: `${name} (region=eu)`, cell_key: '{}', params: { region: 'eu' }, status: 'running', samples_taken: 0, distinct_ips: 0 }],
    })
    const report = { distinct_ips: 0, estimated_pool_size: 0, pool_size_lower_bound: false, honor_rate: null, composition: { mobile: 0, residential: 0, datacenter: 0, unknown: 0 }, risk_histogram: [], flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0, series: [], cells: [], ips: [] }
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (init?.method === 'PATCH') {
        // The server renames the run and cascades to the generated variant name.
        name = (JSON.parse(String(init.body)) as { name: string }).name
        return Promise.resolve(new Response(JSON.stringify(detail().run), { status: 200, headers: { 'Content-Type': 'application/json' } }))
      }
      const body = url.endsWith('/report') ? report : detail()
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    })
    vi.stubGlobal('fetch', fetchMock)
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/runs/r1']}>
          <Routes><Route path="/runs/:id" element={<RunPage />} /></Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    const user = userEvent.setup()

    await screen.findByRole('heading', { level: 1, name: 'Alpha' })
    await user.click(screen.getByRole('button', { name: 'Rename run name' }))
    const input = screen.getByRole('textbox', { name: 'run name' })
    await user.clear(input)
    await user.type(input, 'Beta{Enter}')

    expect(await screen.findByRole('heading', { level: 1, name: 'Beta' })).toBeInTheDocument()
    expect(await screen.findByText('Beta (region=eu)')).toBeInTheDocument()
    const patches = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PATCH')
    expect(patches).toHaveLength(1)
    expect(JSON.parse(String((patches[0][1] as RequestInit).body))).toEqual({ name: 'Beta' })
  })

  it('surfaces a failed run rename without losing the typed name', async () => {
    const detail = { run: { id: 'r1', name: 'Alpha', template_display: 'gate:1080', status: 'running', variant_count: 0, distinct_ips: 0, created_at: new Date().toISOString() }, variants: [] }
    const report = { distinct_ips: 0, estimated_pool_size: 0, pool_size_lower_bound: false, honor_rate: null, composition: { mobile: 0, residential: 0, datacenter: 0, unknown: 0 }, risk_histogram: [], flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0, series: [], cells: [], ips: [] }
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'PATCH') {
        return Promise.resolve(new Response(JSON.stringify({ code: 'invalid_request', message: 'request is invalid' }), { status: 400, statusText: 'Request failed', headers: { 'Content-Type': 'application/json' } }))
      }
      const body = String(input).endsWith('/report') ? report : detail
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/runs/r1']}>
          <Routes><Route path="/runs/:id" element={<RunPage />} /></Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    const user = userEvent.setup()

    await screen.findByRole('heading', { level: 1, name: 'Alpha' })
    await user.click(screen.getByRole('button', { name: 'Rename run name' }))
    const input = screen.getByRole('textbox', { name: 'run name' })
    await user.clear(input)
    await user.type(input, 'Rejected name{Enter}')

    expect(await screen.findByText('Could not rename run')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'run name' })).toHaveValue('Rejected name')
  })
})

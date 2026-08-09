import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
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
})

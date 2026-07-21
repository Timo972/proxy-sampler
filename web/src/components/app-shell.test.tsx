import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { AppShell } from './app-shell'

describe('AppShell', () => {
  it('keeps readiness explicitly named when its visible text is hidden on mobile', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      statusText: 'OK',
      json: async () => ({ postgres: true, clickhouse: true }),
    } as Response))
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })

    render(
      <QueryClientProvider client={queryClient}>
        <AppShell onNewSession={vi.fn()}><h1>Dashboard</h1></AppShell>
      </QueryClientProvider>,
    )

    await waitFor(() => expect(screen.getByRole('status')).toHaveAttribute('aria-label', 'Services ready'))
  })
})

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Session } from '../lib/api'
import { HomePage } from './home-page'

const running = sessionFixture({
  id: '11111111-1111-4111-8111-111111111111',
  name: 'Frankfurt sticky',
  status: 'running',
  mode: 'sticky',
  proxy_display: 'proxy.example:1080',
  success_rate: 0.875,
  last_rtt_ms: 142,
  distinct_ips: 3,
  last_primary_ip: '203.0.113.7',
  last_primary_category: 'residential',
})

const stopped = sessionFixture({
  id: '22222222-2222-4222-8222-222222222222',
  name: 'Rotating pool',
  status: 'stopped',
  mode: 'pool',
  proxy_display: 'pool.example:9000',
})

const newlyCreated = {
  id: '33333333-3333-4333-8333-333333333333',
  name: 'New no-sample session',
  proxy_display: 'new.example:1080',
  mode: 'sticky',
  status: 'running',
  cadence_seconds: 30,
  probes_per_sample: 3,
  probe_target: 'https://speed.cloudflare.com/cdn-cgi/trace',
  dial_timeout_ms: 10000,
  samples_taken: 0,
  probes_ok: 0,
  probes_total: 0,
  success_rate: 0,
  distinct_ips: 0,
  created_at: '2026-07-21T12:00:00Z',
} satisfies Session

afterEach(() => {
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
})

describe('HomePage', () => {
  it('shows stable skeleton rows while sessions load', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => undefined)))
    renderHome()

    expect(screen.getByRole('status', { name: 'Loading sessions' })).toBeInTheDocument()
    expect(screen.getAllByLabelText('Loading session')).toHaveLength(4)
  })

  it('teaches the modes and keeps first-session creation in the page flow', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse([])))
    const onNewSession = vi.fn()
    renderHome(onNewSession)

    expect(await screen.findByRole('heading', { name: 'Start your first sampling session' })).toBeInTheDocument()
    expect(screen.getByText(/Sticky sessions measure how long one egress IP is held/i)).toBeInTheDocument()
    expect(screen.getByText(/Pool sessions measure per-request rotation/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'New session' }))
    expect(onNewSession).toHaveBeenCalledOnce()
  })

  it('separates active and inactive sessions with the required operator fields', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse([running, stopped])))
    const { container } = renderHome()

    const activeHeading = await screen.findByRole('heading', { name: 'Active sessions' })
    expect(activeHeading).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1, name: 'Sampling sessions' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Stopped / Finished sessions' })).toBeInTheDocument()
    const activeSection = activeHeading.closest('section')!
    for (const heading of ['Name', 'Proxy', 'Mode', 'Status', 'Success rate', 'Median RTT', 'Distinct IPs', 'Last IP / category', 'Last sample']) {
      expect(within(activeSection).getByRole('columnheader', { name: heading })).toBeInTheDocument()
    }
    const desktopTable = activeSection.querySelector<HTMLElement>('.session-table-wrap')!
    expect(within(desktopTable).getByText('87.5%')).toBeInTheDocument()
    expect(within(desktopTable).getByText('142 ms')).toBeInTheDocument()

    const mobileRecords = container.querySelector('.session-records')
    expect(mobileRecords).not.toBeNull()
    const labels = Array.from(mobileRecords!.querySelectorAll('dt')).map((node) => node.textContent)
    expect(labels).toEqual(expect.arrayContaining(['Proxy', 'Mode', 'Status', 'Success rate', 'Median RTT', 'Distinct IPs', 'Last IP / category', 'Last sample']))
    expect(container.querySelector('.home-page')).not.toHaveClass('no-page-overflow')
  })

  it('renders a newly created session when nullable sample fields are omitted', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      return jsonResponse([newlyCreated])
    }))
    renderHome()

    const row = await screen.findByRole('row', { name: /New no-sample session/i })
    expect(within(row).getAllByText('—')).toHaveLength(3)
  })

  it('explains when the active group is empty', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse([stopped])))
    renderHome()

    const heading = await screen.findByRole('heading', { name: 'Active sessions' })
    const section = heading.closest('section')!
    expect(within(section).getByText('No sessions are currently running.')).toBeInTheDocument()
    expect(within(section).queryByRole('table')).not.toBeInTheDocument()
  })

  it('explains when the stopped and finished group is empty', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse([running])))
    renderHome()

    const heading = await screen.findByRole('heading', { name: 'Stopped / Finished sessions' })
    const section = heading.closest('section')!
    expect(within(section).getByText('Stopped and finished sessions will appear here.')).toBeInTheDocument()
    expect(within(section).queryByRole('table')).not.toBeInTheDocument()
  })

  it('navigates from a row while stop remains a local row action', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      if (init?.method === 'POST') return emptyResponse()
      return jsonResponse([running])
    })
    vi.stubGlobal('fetch', fetchMock)
    const firstRender = renderHome()

    const row = await screen.findByRole('row', { name: /Frankfurt sticky/i })
    fireEvent.click(row)
    expect(await screen.findByText('Session destination')).toBeInTheDocument()

    firstRender.unmount()
    firstRender.queryClient.clear()
    renderHome()
    const stop = await screen.findByRole('button', { name: 'Stop Frankfurt sticky' })
    fireEvent.click(stop)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      `/api/sessions/${running.id}/stop`,
      expect.objectContaining({ method: 'POST' }),
    ))
    expect(screen.queryByText('Session destination')).not.toBeInTheDocument()
  })

  it('provides a real session link for keyboard and browser navigation', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      return jsonResponse([running])
    }))
    renderHome()

    const links = await screen.findAllByRole('link', { name: 'Frankfurt sticky' })
    expect(links).toHaveLength(2)
    for (const link of links) expect(link).toHaveAttribute('href', `/sessions/${running.id}`)
  })

  it.each(['{Enter}', ' '])('keeps Stop keyboard activation local for %s', async (key) => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      if (init?.method === 'POST') return emptyResponse()
      return jsonResponse([running])
    })
    vi.stubGlobal('fetch', fetchMock)
    renderHome()
    const user = userEvent.setup()

    const stop = await screen.findByRole('button', { name: 'Stop Frankfurt sticky' })
    stop.focus()
    await user.keyboard(key)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      `/api/sessions/${running.id}/stop`,
      expect.objectContaining({ method: 'POST' }),
    ))
    expect(screen.queryByText('Session destination')).not.toBeInTheDocument()
  })

  it('keeps dependency errors inline and retries from the same surface', async () => {
    let sessionsCalls = 0
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      sessionsCalls += 1
      return sessionsCalls === 1
        ? errorResponse(503, 'dependency_unavailable', 'Dependency unavailable')
        : jsonResponse([running])
    })
    vi.stubGlobal('fetch', fetchMock)
    renderHome()

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Dependency unavailable')
    await userEvent.click(within(alert).getByRole('button', { name: 'Retry' }))
    expect((await screen.findAllByText('Frankfurt sticky')).length).toBeGreaterThan(0)
    expect(sessionsCalls).toBe(2)
  })

  it('keeps the runs UI usable when the sessions query fails', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      return errorResponse(503, 'dependency_unavailable', 'Dependency unavailable')
    })
    vi.stubGlobal('fetch', fetchMock)
    renderHome()

    // The sessions failure is surfaced, but the runs section — including the
    // only "New run" button — stays reachable.
    expect(await screen.findByText('Sessions are unavailable')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Parameter-variation runs' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New run' })).toBeInTheDocument()
  })

  it('polls every five seconds only for visible running sessions and cleans up', async () => {
    vi.useFakeTimers()
    const add = vi.spyOn(document, 'addEventListener')
    const remove = vi.spyOn(document, 'removeEventListener')
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      return jsonResponse([running])
    })
    vi.stubGlobal('fetch', fetchMock)
    const { unmount } = renderHome()

    await settleQueries()
    expect(sessionCallCount(fetchMock)).toBe(1)
    await act(async () => vi.advanceTimersByTime(4999))
    expect(sessionCallCount(fetchMock)).toBe(1)
    await act(async () => vi.advanceTimersByTime(1))
    await settleQueries()
    expect(sessionCallCount(fetchMock)).toBe(2)

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    await act(async () => vi.advanceTimersByTime(10000))
    expect(sessionCallCount(fetchMock)).toBe(2)

    unmount()
    expect(add).toHaveBeenCalledWith('visibilitychange', expect.any(Function))
    expect(remove).toHaveBeenCalledWith('visibilitychange', expect.any(Function))
    await act(async () => vi.advanceTimersByTime(10000))
    expect(sessionCallCount(fetchMock)).toBe(2)
  })

  it('does not poll when every session is stopped', async () => {
    vi.useFakeTimers()
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/runs')) return jsonResponse([])
      return jsonResponse([stopped])
    })
    vi.stubGlobal('fetch', fetchMock)
    renderHome()
    await settleQueries()
    await act(async () => vi.advanceTimersByTime(15000))
    expect(sessionCallCount(fetchMock)).toBe(1)
  })
})

function renderHome(onNewSession = vi.fn(), onNewRun = vi.fn()) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  const result = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={['/']}>
        <Routes>
          <Route path="/" element={<HomePage onNewSession={onNewSession} onNewRun={onNewRun} />} />
          <Route path="/sessions/:id" element={<p>Session destination</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  )
  return { ...result, queryClient }
}

async function settleQueries() {
  await act(async () => {
    await Promise.resolve()
    await Promise.resolve()
  })
}

function sessionCallCount(mock: ReturnType<typeof vi.fn>) {
  return mock.mock.calls.filter(([input]) => String(input).startsWith('/api/sessions')).length
}

function sessionFixture(overrides: Partial<Session> = {}): Session {
  return {
    id: '00000000-0000-4000-8000-000000000000',
    name: 'Session',
    proxy_display: 'proxy.example:1080',
    mode: 'sticky',
    status: 'finished',
    cadence_seconds: 30,
    probes_per_sample: 3,
    probe_target: 'https://speed.cloudflare.com/cdn-cgi/trace',
    dial_timeout_ms: 10000,
    max_samples: null,
    max_duration_seconds: null,
    samples_taken: 12,
    probes_ok: 30,
    probes_total: 36,
    success_rate: 30 / 36,
    distinct_ips: 1,
    last_sample_at: '2026-07-21T12:00:00Z',
    last_primary_ip: '203.0.113.1',
    last_primary_category: 'residential',
    last_rtt_ms: 120,
    last_error: null,
    created_at: '2026-07-21T10:00:00Z',
    started_at: '2026-07-21T10:00:00Z',
    stopped_at: null,
    ...overrides,
  }
}

function jsonResponse(value: unknown): Response {
  return { ok: true, status: 200, statusText: 'OK', json: async () => value } as Response
}

function emptyResponse(): Response {
  return { ok: true, status: 204, statusText: 'No Content', json: async () => undefined } as Response
}

function errorResponse(status: number, code: string, message: string): Response {
  return { ok: false, status, statusText: message, json: async () => ({ code, message }) } as Response
}

import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Session } from '../lib/api'
import { SessionPage } from './session-page'

vi.mock('recharts', () => {
  const Container = ({ children }: { children?: ReactNode }) => <div>{children}</div>
  const Empty = () => null
  return {
    Area: Empty,
    AreaChart: Container,
    Bar: Empty,
    BarChart: Container,
    CartesianGrid: Empty,
    Cell: Empty,
    Legend: Empty,
    Line: Empty,
    LineChart: Container,
    Pie: Empty,
    PieChart: Container,
    ReferenceLine: Empty,
    ResponsiveContainer: Container,
    Tooltip: Empty,
    XAxis: Empty,
    YAxis: Empty,
  }
})

const sessionID = '11111111-1111-4111-8111-111111111111'

const runningSession = {
  id: sessionID,
  name: 'Frankfurt sticky',
  proxy_display: 'proxy.example:1080',
  mode: 'sticky',
  status: 'running',
  cadence_seconds: 30,
  probes_per_sample: 3,
  probe_target: 'https://speed.cloudflare.com/cdn-cgi/trace',
  dial_timeout_ms: 10000,
  samples_taken: 12,
  probes_ok: 31,
  probes_total: 36,
  success_rate: 31 / 36,
  distinct_ips: 3,
  last_sample_at: '2026-07-21T12:04:00Z',
  last_primary_ip: '203.0.113.7',
  last_primary_category: 'residential',
  last_rtt_ms: 142,
  created_at: '2026-07-21T12:00:00Z',
  started_at: '2026-07-21T12:00:00Z',
} satisfies Session

const report = {
  series: [
    { at: '2026-07-21T12:00:00Z', success_rate: 2 / 3, latency_p50_ms: 120, latency_p95_ms: 190, distinct_per_sample: 1, ip_changes: 0, mobile: 0, residential: 1, datacenter: 0, unknown: 0 },
    { at: '2026-07-21T12:01:00Z', success_rate: 1, latency_p50_ms: 140, latency_p95_ms: 230, distinct_per_sample: 2, ip_changes: 1, mobile: 0, residential: 1, datacenter: 1, unknown: 0 },
  ],
  stickiness: {
    holds: [{ ip: '203.0.113.7', started_at: '2026-07-21T12:00:00Z', ended_at: '2026-07-21T12:01:00Z', samples: 2, duration_seconds: 60 }],
    rotations: [{ at: '2026-07-21T12:01:00Z', from_ip: '203.0.113.7', to_ip: '198.51.100.9', since_previous_seconds: 60 }],
    average_hold_seconds: 60,
    median_hold_seconds: 60,
  },
  pool_growth: [
    { at: '2026-07-21T12:00:00Z', distinct_ips: 1 },
    { at: '2026-07-21T12:01:00Z', distinct_ips: 3 },
  ],
  pool_composition: { mobile: 0, residential: 2, datacenter: 1, unknown: 0 },
  reputation_summary: { total_ips: 3, flagged_ips: 1, flagged_percent: 100 / 3, dnsbl_hit_ips: 1 },
  risk_histogram: [
    { label: '0–29', min: 0, max: 29, count: 1 },
    { label: '70–100', min: 70, max: 100, count: 1 },
  ],
  ips: [
  { ip: '198.51.100.9', category: 'datacenter', country: 'DE', isp: 'Example Transit', asn: 'AS64501 Example Transit', risk_score: 82, greynoise_class: 'malicious', dnsbl_listed: true, dnsbl_hits: ['zen.spamhaus.org'], first_seen: '2026-07-21T12:01:00Z', last_seen: '2026-07-21T12:04:00Z', hit_count: 8 },
  { ip: '203.0.113.7', category: 'residential', country: 'DE', isp: 'Example Broadband', asn: 'AS64500 Example Broadband', risk_score: 12, greynoise_class: 'benign', dnsbl_listed: false, dnsbl_hits: [], first_seen: '2026-07-21T12:00:00Z', last_seen: '2026-07-21T12:03:00Z', hit_count: 4 },
  ],
}

const samplePage = {
  items: [{
    sample_seq: 12,
    sampled_at: '2026-07-21T12:04:00Z',
    probes_attempted: 3,
    probes_ok: 2,
    primary_ip: '203.0.113.7',
    distinct_ips: 2,
    ip_changed: true,
    new_ips: 1,
    rtt_min_ms: 101,
    rtt_med_ms: 142,
    rtt_max_ms: 205,
    egress_country: 'DE',
    primary_category: 'residential',
    primary_risk: 12,
    probe_ips: ['203.0.113.7', null, '198.51.100.9'],
    probe_rtts_ms: [101, 0, 205],
    probe_ok: [true, false, true],
    error: 'probe 1 timed out',
  }],
  page: 1,
  page_size: 50,
  total: 1,
}

afterEach(() => {
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
})

describe('SessionPage', () => {
  it('shows stable detail, report, and sample skeletons while the selected route loads', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => undefined)))
    renderSession('?tab=samples&page=1')

    expect(screen.getByRole('status', { name: 'Loading session detail' })).toBeInTheDocument()
    expect(screen.getByLabelText('Loading report')).toBeInTheDocument()
    expect(screen.getByLabelText('Loading samples')).toBeInTheDocument()
  })

  it('renders the operator hierarchy, one summary strip, truthful chart sections, and export', async () => {
    vi.stubGlobal('fetch', createFetch())
    const { container } = renderSession()

    expect(await screen.findByRole('heading', { level: 1, name: 'Frankfurt sticky' })).toBeInTheDocument()
    expect(screen.getByText('proxy.example:1080')).toBeInTheDocument()
    expect(screen.getByText('Sticky')).toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
    const summary = screen.getByRole('region', { name: 'Session summary' })
    expect(within(summary).getByText('86.1%')).toBeInTheDocument()
    expect(within(summary).getByText('142 ms')).toBeInTheDocument()
    expect(within(summary).getByText(/203\.0\.113\.7/)).toBeInTheDocument()
    expect(container.querySelectorAll('.summary-strip')).toHaveLength(1)
    expect(container.querySelector('.metric-card')).toBeNull()

    for (const name of ['Success rate', 'Latency', 'Composition over time', 'Pool composition', 'Stickiness', 'Pool growth']) {
      expect(screen.getByRole('heading', { name })).toBeInTheDocument()
    }
    expect(screen.getByText(/Success ranged from/i)).toBeInTheDocument()
    expect(screen.getByText(/Hold segments show/i)).toBeInTheDocument()
    expect(screen.getByText(/Last updated/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Export CSV' })).toHaveAttribute('href', `/api/sessions/${sessionID}/export.csv`)
  })

  it('keeps partial reports usable with specific empty-state explanations', async () => {
    const emptyReport = {
      ...report,
      series: [],
      stickiness: { holds: [], rotations: [], average_hold_seconds: 0, median_hold_seconds: 0 },
      pool_growth: [],
      pool_composition: { mobile: 0, residential: 0, datacenter: 0, unknown: 0 },
      risk_histogram: [],
      ips: [],
    }
    vi.stubGlobal('fetch', createFetch({ report: emptyReport, samples: { ...samplePage, items: [], total: 0 } }))
    renderSession()

    await screen.findByRole('heading', { name: 'Frankfurt sticky' })
    expect(screen.getByText('Success data will appear after the first completed sample.')).toBeInTheDocument()
    expect(screen.getByText('Latency data will appear after a successful probe.')).toBeInTheDocument()
    expect(screen.getByText('Composition appears once an egress IP is classified.')).toBeInTheDocument()
  expect(screen.getByText('Pool composition appears once an egress IP is classified.')).toBeInTheDocument()
    expect(screen.getByText('Stickiness segments appear after an IP has been observed.')).toBeInTheDocument()
    expect(screen.getByText('Pool growth appears after an egress IP is observed.')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('tab', { name: 'Reputation' }))
    expect(screen.getByText('Risk distribution appears after reputation enrichment.')).toBeInTheDocument()
    expect(screen.getByText('IP reputation rows appear after an egress IP is observed.')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'Samples' }))
    expect(await screen.findByText('Samples will appear after the first sampling interval completes.')).toBeInTheDocument()
  })

  it('offers a real route home when the session does not exist', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ code: 'not_found', message: 'session not found' }, 404)))
    renderSession()

    expect(await screen.findByRole('heading', { name: 'Session not found' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('link', { name: 'Back to sessions' }))
    expect(screen.getByText('Home destination')).toBeInTheDocument()
  })

  it('shows a local report error and retries without hiding the session identity', async () => {
    let reportAttempts = 0
    const fetchMock = createFetch({
      reportResponse: () => {
        reportAttempts += 1
        return reportAttempts === 1
          ? jsonResponse({ code: 'report_failed', message: 'report unavailable' }, 503)
          : jsonResponse(report)
      },
    })
    vi.stubGlobal('fetch', fetchMock)
    renderSession()

    expect(await screen.findByRole('heading', { name: 'Frankfurt sticky' })).toBeInTheDocument()
    expect(await screen.findByRole('alert')).toHaveTextContent('report unavailable')
    await userEvent.click(screen.getByRole('button', { name: 'Retry report' }))
    expect(await screen.findByRole('heading', { name: 'Success rate' })).toBeInTheDocument()
    expect(reportAttempts).toBe(2)
  })

  it('polls a running visible report every five seconds, pauses while hidden, and cleans up listeners', async () => {
    vi.useFakeTimers()
    const addSpy = vi.spyOn(document, 'addEventListener')
    const removeSpy = vi.spyOn(document, 'removeEventListener')
    const fetchMock = createFetch()
    vi.stubGlobal('fetch', fetchMock)
    const view = renderSession()

    await act(async () => { await Promise.resolve(); await Promise.resolve() })
    const initialReports = callsFor(fetchMock, `/api/sessions/${sessionID}/report`)
    expect(initialReports).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(2)

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    await act(async () => { await vi.advanceTimersByTimeAsync(10000) })
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(2)

    view.unmount()
    expect(addSpy.mock.calls.filter(([name]) => name === 'visibilitychange').length).toBeGreaterThan(0)
    expect(removeSpy.mock.calls.filter(([name]) => name === 'visibilitychange')).toHaveLength(
      addSpy.mock.calls.filter(([name]) => name === 'visibilitychange').length,
    )
  })

  it('polls only the exact selected sample page with detail and report, then stops all work when hidden or unmounted', async () => {
  vi.useFakeTimers()
  const fetchMock = createFetch({ samples: { ...samplePage, page: 2 } })
  vi.stubGlobal('fetch', fetchMock)
  const view = renderSession('?tab=samples&page=2')

  await act(async () => { await Promise.resolve(); await Promise.resolve() })
  const sampleURL = `/api/sessions/${sessionID}/samples?page=2&page_size=50`
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBe(1)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(1)
  expect(callsFor(fetchMock, sampleURL)).toBe(1)
  expect(fetchMock.mock.calls.filter(([input]) => String(input).includes('/samples')).map(([input]) => String(input))).toEqual([sampleURL])

  await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBe(2)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(2)
  expect(callsFor(fetchMock, sampleURL)).toBe(2)

  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
  act(() => document.dispatchEvent(new Event('visibilitychange')))
  await act(async () => { await vi.advanceTimersByTimeAsync(10000) })
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBe(2)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(2)
  expect(callsFor(fetchMock, sampleURL)).toBe(2)

  view.unmount()
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
  act(() => document.dispatchEvent(new Event('visibilitychange')))
  await act(async () => { await vi.advanceTimersByTimeAsync(10000) })
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBe(2)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(2)
  expect(callsFor(fetchMock, sampleURL)).toBe(2)
  })

  it('does not poll stopped sessions', async () => {
    vi.useFakeTimers()
    const fetchMock = createFetch({ session: { ...runningSession, status: 'stopped' } })
    vi.stubGlobal('fetch', fetchMock)
  renderSession('?tab=samples&page=1')
    await act(async () => { await Promise.resolve(); await Promise.resolve() })
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBe(1)
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(1)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/samples?page=1&page_size=50`)).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(15000) })
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBe(1)
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBe(1)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/samples?page=1&page_size=50`)).toBe(1)
  })

  it('confirms stop, then invalidates detail and report queries', async () => {
    let stopped = false
    const baseFetch = createFetch({ session: () => ({ ...runningSession, status: stopped ? 'stopped' : 'running' }) })
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith('/stop')) {
        stopped = true
        return emptyResponse()
      }
      return baseFetch(input, init)
    })
    const confirm = vi.fn().mockReturnValueOnce(false).mockReturnValueOnce(true)
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('confirm', confirm)
  renderSession('?tab=samples&page=1')

    const stop = await screen.findByRole('button', { name: 'Stop session' })
    await userEvent.click(stop)
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/stop`)).toBe(0)
    await userEvent.click(stop)
    await waitFor(() => expect(callsFor(fetchMock, `/api/sessions/${sessionID}/stop`)).toBe(1))
    await waitFor(() => expect(screen.getByText('Stopped')).toBeInTheDocument())
    expect(confirm).toHaveBeenCalledTimes(2)
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}`)).toBeGreaterThan(1)
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/report`)).toBeGreaterThan(1)
  expect(callsFor(fetchMock, `/api/sessions/${sessionID}/samples?page=1&page_size=50`)).toBeGreaterThan(1)
  })

  it('keeps tab and sample page state in browser history and only fetches the selected page', async () => {
    const fetchMock = createFetch()
    vi.stubGlobal('fetch', fetchMock)
    renderSession()
    await screen.findByRole('heading', { name: 'Frankfurt sticky' })

    await userEvent.click(screen.getByRole('tab', { name: 'Reputation' }))
    expect(screen.getByTestId('location')).toHaveTextContent(`/${sessionID}?tab=reputation`)
    await userEvent.click(screen.getByRole('tab', { name: 'Samples' }))
    expect(screen.getByTestId('location')).toHaveTextContent(`/${sessionID}?tab=samples&page=1`)
    expect(await screen.findByText('Sample 12')).toBeInTheDocument()
    expect(fetchMock.mock.calls.filter(([input]) => String(input).includes('/samples')).map(([input]) => String(input))).toEqual([
      `/api/sessions/${sessionID}/samples?page=1&page_size=50`,
    ])

    await userEvent.click(screen.getByRole('button', { name: 'Browser back' }))
    expect(screen.getByTestId('location')).toHaveTextContent(`/${sessionID}?tab=reputation`)
    expect(callsFor(fetchMock, `/api/sessions/${sessionID}/samples?page=1&page_size=50`)).toBe(1)
  })
})

function renderSession(search = '') {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } })
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[`/sessions/${sessionID}${search}`]}>
        <Routes>
          <Route path="/" element={<p>Home destination</p>} />
          <Route path="/sessions/:id" element={<><SessionPage /><RouteState /></>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  )
  return { ...view, queryClient }
}

function RouteState() {
  const location = useLocation()
  const navigate = useNavigate()
  return <><output data-testid="location">{location.pathname}{location.search}</output><button type="button" onClick={() => navigate(-1)}>Browser back</button></>
}

function createFetch(options: {
  session?: Session | (() => Session)
  report?: typeof report
  samples?: typeof samplePage
  reportResponse?: () => Response
} = {}) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url === `/api/sessions/${sessionID}/report`) return options.reportResponse?.() ?? jsonResponse(options.report ?? report)
    if (url.startsWith(`/api/sessions/${sessionID}/samples`)) return jsonResponse(options.samples ?? samplePage)
    if (url === `/api/sessions/${sessionID}/stop` && init?.method === 'POST') return emptyResponse()
    if (url === `/api/sessions/${sessionID}`) {
      const value = typeof options.session === 'function' ? options.session() : options.session
      return jsonResponse(value ?? runningSession)
    }
    throw new Error(`Unexpected request: ${url}`)
  })
}

function callsFor(mock: ReturnType<typeof vi.fn>, exactURL: string) {
  return mock.mock.calls.filter(([input]) => String(input) === exactURL).length
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    statusText: status === 404 ? 'Not Found' : status >= 400 ? 'Request failed' : 'OK',
    headers: { 'Content-Type': 'application/json' },
  })
}

function emptyResponse() {
  return new Response(null, { status: 204 })
}

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'

import type { Session } from '../lib/api'
import { NewSessionDialog } from './new-session-dialog'

describe('NewSessionDialog', () => {
  it('requires name, proxy, mode, and cadence', async () => {
    vi.stubGlobal('fetch', vi.fn())
    renderDialog()
    const user = userEvent.setup()

    await user.clear(screen.getByLabelText('Cadence (seconds)'))
    await user.click(screen.getByRole('button', { name: 'Start session' }))

    expect(await screen.findByText('Name is required')).toBeInTheDocument()
    expect(screen.getByText('Proxy connection string is required')).toBeInTheDocument()
    expect(screen.getByText('Choose a sampling mode')).toBeInTheDocument()
    expect(screen.getByText('Cadence must be a positive integer')).toBeInTheDocument()
    expect(screen.getByLabelText('Session name')).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByLabelText('Session name')).toHaveAccessibleDescription('Name is required')
    expect(fetch).not.toHaveBeenCalled()
  })

  it('applies mode probe defaults only until probes are manually edited', async () => {
    renderDialog()
    const user = userEvent.setup()
    const mode = screen.getByLabelText('Sampling mode')
    const probes = screen.getByLabelText('Probes per sample')

    await user.selectOptions(mode, 'sticky')
    expect(probes).toHaveValue(3)
    await user.selectOptions(mode, 'pool')
    expect(probes).toHaveValue(8)
    await user.clear(probes)
    await user.type(probes, '5')
    await user.selectOptions(mode, 'sticky')
    expect(probes).toHaveValue(5)
  })

  it('accepts blank or positive caps and rejects zero', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(createdSession)))
    renderDialog()
    const user = userEvent.setup()
    await fillRequired(user)
    await user.click(screen.getByRole('button', { name: 'Advanced settings' }))
    const maxSamples = screen.getByLabelText('Maximum samples (optional)')
    const maxDuration = screen.getByLabelText('Maximum duration (seconds, optional)')

    await user.type(maxSamples, '0')
    await user.type(maxDuration, '0')
    await user.click(screen.getByRole('button', { name: 'Start session' }))
    expect(await screen.findAllByText('Enter a positive integer or leave blank')).toHaveLength(2)
    expect(fetch).not.toHaveBeenCalled()

    await user.clear(maxSamples)
    await user.clear(maxDuration)
    await user.type(maxSamples, '100')
    await user.type(maxDuration, '3600')
    await user.click(screen.getByRole('button', { name: 'Start session' }))
    await waitFor(() => expect(fetch).toHaveBeenCalledOnce())
    const body = JSON.parse((vi.mocked(fetch).mock.calls[0][1] as RequestInit).body as string)
    expect(body.max_samples).toBe(100)
    expect(body.max_duration_seconds).toBe(3600)
  })

  it('clears the secret immediately, disables controls, then closes and navigates on success', async () => {
    let resolveRequest!: (response: Response) => void
    const fetchMock = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => new Promise<Response>((resolve) => { resolveRequest = resolve }))
    vi.stubGlobal('fetch', fetchMock)
    renderDialog()
    const user = userEvent.setup()
    const secret = 'socks5h://user:top-secret@proxy.example:1080'
    await fillRequired(user, secret)

    await user.click(screen.getByRole('button', { name: 'Start session' }))
    expect(screen.getByLabelText('Proxy connection string')).toHaveValue('')
    expect(screen.getByLabelText('Session name')).toBeDisabled()
    expect(screen.getByLabelText('Sampling mode')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Starting session…' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Close dialog' })).toBeDisabled()
    expect(document.body).not.toHaveTextContent(secret)

    resolveRequest(jsonResponse(createdSession))
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent(`/sessions/${createdSession.id}`))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    const body = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string)
    expect(body.proxy).toBe(secret)
  })

  it('keeps server validation in the dialog without retaining or echoing the secret', async () => {
    const secret = 'http://operator:hidden-password@proxy.example:8080'
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(errorResponse(400, 'invalid_request', 'The proxy URL is invalid')))
    renderDialog()
    const user = userEvent.setup()
    await fillRequired(user, secret)

    await user.click(screen.getByRole('button', { name: 'Start session' }))

    const dialog = await screen.findByRole('dialog', { name: 'New sampling session' })
    expect(dialog).toHaveTextContent('The proxy URL is invalid')
    expect(screen.getByLabelText('Proxy connection string')).toHaveValue('')
    expect(document.body).not.toHaveTextContent(secret)
    expect(screen.getByTestId('location')).toHaveTextContent('/')
  })

  it('uses an accessible modal with initial focus and Escape close', async () => {
    renderDialog()
    const user = userEvent.setup()

    expect(await screen.findByRole('dialog', { name: 'New sampling session' })).toBeInTheDocument()
    await waitFor(() => expect(screen.getByLabelText('Session name')).toHaveFocus())
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})

function renderDialog() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={['/']}>
        <DialogHarness />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

function DialogHarness() {
  const [open, setOpen] = useState(true)
  return (
    <>
      <Routes>
        <Route path="*" element={<Location />} />
      </Routes>
      <NewSessionDialog open={open} onOpenChange={setOpen} />
    </>
  )
}

function Location() {
  return <output data-testid="location">{useLocation().pathname}</output>
}

async function fillRequired(user: ReturnType<typeof userEvent.setup>, proxy = 'socks5h://user:password@proxy.example:1080') {
  await user.type(screen.getByLabelText('Session name'), 'Provider evaluation')
  await user.type(screen.getByLabelText('Proxy connection string'), proxy)
  await user.selectOptions(screen.getByLabelText('Sampling mode'), 'sticky')
}

const createdSession: Session = {
  id: '33333333-3333-4333-8333-333333333333',
  name: 'Provider evaluation',
  proxy_display: 'proxy.example:1080',
  mode: 'sticky',
  status: 'running',
  cadence_seconds: 30,
  probes_per_sample: 3,
  probe_target: 'https://speed.cloudflare.com/cdn-cgi/trace',
  dial_timeout_ms: 10000,
  max_samples: null,
  max_duration_seconds: null,
  samples_taken: 0,
  probes_ok: 0,
  probes_total: 0,
  success_rate: 0,
  distinct_ips: 0,
  last_sample_at: null,
  last_primary_ip: null,
  last_primary_category: null,
  last_rtt_ms: null,
  last_error: null,
  created_at: '2026-07-21T12:00:00Z',
  started_at: '2026-07-21T12:00:00Z',
  stopped_at: null,
}

function jsonResponse(value: unknown): Response {
  return { ok: true, status: 201, statusText: 'Created', json: async () => value } as Response
}

function errorResponse(status: number, code: string, message: string): Response {
  return { ok: false, status, statusText: message, json: async () => ({ code, message }) } as Response
}

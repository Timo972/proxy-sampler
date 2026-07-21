import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

export type SessionMode = 'sticky' | 'pool'
export type SessionStatus = 'running' | 'stopped' | 'finished'

export interface Session {
  id: string
  name: string
  proxy_display: string
  mode: SessionMode
  status: SessionStatus
  cadence_seconds: number
  probes_per_sample: number
  probe_target: string
  dial_timeout_ms: number
  max_samples?: number | null
  max_duration_seconds?: number | null
  samples_taken: number
  probes_ok: number
  probes_total: number
  success_rate: number
  distinct_ips: number
  last_sample_at?: string | null
  last_primary_ip?: string | null
  last_primary_category?: string | null
  last_rtt_ms?: number | null
  last_error?: string | null
  created_at: string
  started_at?: string | null
  stopped_at?: string | null
}

export interface CreateSessionRequest {
  name: string
  proxy: string
  mode: SessionMode
  cadence_seconds: number
  probes_per_sample?: number
  probe_target?: string
  dial_timeout_ms?: number
  max_samples?: number | null
  max_duration_seconds?: number | null
}

export interface Readiness {
  postgres: boolean
  clickhouse: boolean
}

export class APIError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
    this.name = 'APIError'
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init?.headers },
  })
  if (!response.ok) {
    const payload: unknown = await response.json().catch(() => null)
    const body = isErrorBody(payload) ? payload : undefined
    throw new APIError(response.status, body?.code ?? 'request_failed', (body?.message ?? response.statusText) || 'Request failed')
  }
  return response.status === 204 ? undefined as T : response.json() as Promise<T>
}

function isErrorBody(value: unknown): value is { code?: string; message?: string } {
  if (typeof value !== 'object' || value === null) return false
  const body = value as Record<string, unknown>
  return (body.code === undefined || typeof body.code === 'string')
    && (body.message === undefined || typeof body.message === 'string')
}

export function useSessions() {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['sessions'],
    queryFn: () => api<Session[]>('/api/sessions'),
    refetchInterval: (query) => visible && query.state.data?.some((session) => session.status === 'running') ? 5000 : false,
    refetchIntervalInBackground: false,
  })
}

export function useStopSession() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api<void>(`/api/sessions/${id}/stop`, { method: 'POST' }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['sessions'] }),
  })
}

export function useCreateSession() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (request: CreateSessionRequest) => api<Session>('/api/sessions', {
      method: 'POST',
      body: JSON.stringify(request),
    }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['sessions'] }),
  })
}

export function useReadiness() {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['readiness'],
    queryFn: () => api<Readiness>('/readyz'),
    refetchInterval: visible ? 15000 : false,
    retry: false,
  })
}

function useDocumentVisible() {
  const [visible, setVisible] = useState(() => document.visibilityState === 'visible')
  useEffect(() => {
    const update = () => setVisible(document.visibilityState === 'visible')
    document.addEventListener('visibilitychange', update)
    return () => document.removeEventListener('visibilitychange', update)
  }, [])
  return visible
}

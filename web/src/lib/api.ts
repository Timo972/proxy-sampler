import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

export type SessionMode = 'sticky' | 'pool'
export type SessionStatus = 'running' | 'stopped' | 'finished'

export interface Session {
  id: string
  name: string
  proxy_display: string
  proxy_username?: string | null
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

export interface SeriesPoint {
  at: string
  success_rate: number
  latency_p50_ms: number
  latency_p95_ms: number
  distinct_per_sample: number
  ip_changes: number
  mobile: number
  residential: number
  datacenter: number
  unknown: number
}

export interface StickinessHold {
  ip: string
  started_at: string
  ended_at: string
  samples: number
  duration_seconds: number
}

export interface StickinessRotation {
  at: string
  from_ip: string
  to_ip: string
  since_previous_seconds: number
}

export interface Stickiness {
  holds: StickinessHold[]
  rotations: StickinessRotation[]
  average_hold_seconds: number
  median_hold_seconds: number
}

export interface PoolGrowthPoint {
  at: string
  distinct_ips: number
}

export interface PoolComposition {
  mobile: number
  residential: number
  datacenter: number
  unknown: number
}

export interface ReputationSummary {
  total_ips: number
  flagged_ips: number
  flagged_percent: number
  dnsbl_hit_ips: number
}

export interface RiskBucket {
  label: string
  min: number
  max: number
  count: number
}

export interface IPRow {
  ip: string
  category: string
  country: string
  isp: string
  asn: string
  risk_score: number | null
  greynoise_class: string
  dnsbl_listed: boolean
  dnsbl_hits: string[]
  first_seen: string
  last_seen: string
  hit_count: number
}

export interface SessionReport {
  series: SeriesPoint[]
  stickiness: Stickiness
  pool_growth: PoolGrowthPoint[]
  pool_composition: PoolComposition
  reputation_summary: ReputationSummary
  risk_histogram: RiskBucket[]
  ips: IPRow[]
}

export interface SampleEvent {
  sample_seq: number
  sampled_at: string
  probes_attempted: number
  probes_ok: number
  primary_ip?: string | null
  distinct_ips: number
  ip_changed: boolean
  new_ips: number
  rtt_min_ms: number
  rtt_med_ms: number
  rtt_max_ms: number
  egress_country?: string
  primary_category: string
  primary_risk: number
  probe_ips: Array<string | null>
  probe_rtts_ms: number[]
  probe_ok: boolean[]
  error: string
}

export interface SamplePage {
  items: SampleEvent[]
  page: number
  page_size: number
  total: number
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
    onSuccess: async (_data, id) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['sessions'] }),
        queryClient.invalidateQueries({ queryKey: ['session-report', id] }),
        queryClient.invalidateQueries({ queryKey: ['session-samples', id] }),
      ])
    },
  })
}

export function useSession(id: string) {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['sessions', id],
    queryFn: () => api<Session>(`/api/sessions/${id}`),
    refetchInterval: (query) => visible && query.state.data?.status === 'running' ? 5000 : false,
    refetchIntervalInBackground: false,
    enabled: Boolean(id),
  })
}

export function useSessionReport(id: string, running: boolean) {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['session-report', id],
    queryFn: () => api<SessionReport>(`/api/sessions/${id}/report`),
    refetchInterval: visible && running ? 5000 : false,
    refetchIntervalInBackground: false,
    enabled: Boolean(id),
  })
}

export function useSessionSamples(id: string, page: number, enabled: boolean, running: boolean) {
  const visible = useDocumentVisible()
  return useQuery({
    queryKey: ['session-samples', id, page],
    queryFn: () => api<SamplePage>(`/api/sessions/${id}/samples?page=${page}&page_size=50`),
    refetchInterval: visible && running ? 5000 : false,
    refetchIntervalInBackground: false,
    enabled: Boolean(id) && enabled,
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

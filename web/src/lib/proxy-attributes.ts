import type { IPRow, SessionReport } from './api'

export type AttributeCategory = 'geo' | 'session' | 'network' | 'other'

export interface ParsedAttribute {
  category: AttributeCategory
  key: string
  label: string
  value: string
}

export interface ParsedAttributes {
  attributes: ParsedAttribute[]
  raw: string[]
}

export interface ComparisonRow {
  label: string
  requested: string
  observed: string
  verdict: 'match' | 'partial' | 'mismatch' | 'unknown'
}

interface KeySpec {
  label: string
  category: AttributeCategory
}

// Own-property lookup that ignores inherited Object.prototype members
// (constructor, toString, valueOf, hasOwnProperty, __proto__, ...), which
// would otherwise resolve truthy via the prototype chain on plain-object maps.
function lookup<T>(map: Record<string, T>, key: string): T | undefined {
  return Object.prototype.hasOwnProperty.call(map, key) ? map[key] : undefined
}

// Recognized attribute keys and their aliases.
const KEYS: Record<string, KeySpec> = {
  country: { label: 'Country', category: 'geo' },
  cc: { label: 'Country', category: 'geo' },
  region: { label: 'Region', category: 'geo' },
  state: { label: 'State', category: 'geo' },
  st: { label: 'State', category: 'geo' },
  city: { label: 'City', category: 'geo' },
  session: { label: 'Session', category: 'session' },
  sess: { label: 'Session', category: 'session' },
  sid: { label: 'Session', category: 'session' },
  sticky: { label: 'Sticky', category: 'session' },
  rotate: { label: 'Rotation', category: 'session' },
  rotating: { label: 'Rotation', category: 'session' },
  ttl: { label: 'Session TTL', category: 'session' },
  sesstime: { label: 'Session TTL', category: 'session' },
  zone: { label: 'Network type', category: 'network' },
  type: { label: 'Network type', category: 'network' },
  isp: { label: 'ISP', category: 'network' },
  asn: { label: 'ASN', category: 'network' },
}

const CANONICAL_KEY: Record<string, string> = {
  cc: 'country', st: 'state', sess: 'session', sid: 'session',
  rotating: 'rotate', sesstime: 'ttl', zone: 'type',
}

export function parseProxyAttributes(username: string | null | undefined): ParsedAttributes {
  if (!username) return { attributes: [], raw: [] }
  const tokens = username.split(/[-_:]+/).filter((t) => t.length > 0)
  const attributes: ParsedAttribute[] = []
  const raw: string[] = []
  for (let i = 0; i < tokens.length; i++) {
    const key = tokens[i].toLowerCase()
    const spec = lookup(KEYS, key)
    if (spec && i + 1 < tokens.length) {
      attributes.push({ category: spec.category, key: lookup(CANONICAL_KEY, key) ?? key, label: spec.label, value: tokens[i + 1] })
      i++
      continue
    }
    raw.push(tokens[i])
  }
  return { attributes, raw }
}

function attribute(parsed: ParsedAttributes, key: string): ParsedAttribute | undefined {
  return parsed.attributes.find((a) => a.key === key)
}

function dominantCountry(ips: IPRow[]): { code: string; share: number } | undefined {
  if (ips.length === 0) return undefined
  const totals = new Map<string, number>()
  let sum = 0
  for (const ip of ips) {
    if (!ip.country) continue
    const weight = ip.hit_count > 0 ? ip.hit_count : 1
    totals.set(ip.country, (totals.get(ip.country) ?? 0) + weight)
    sum += weight
  }
  if (sum === 0) return undefined
  const [code, count] = [...totals.entries()].sort((a, b) => b[1] - a[1])[0]
  return { code, share: count / sum }
}

function dominantISP(ips: IPRow[]): string | undefined {
  if (ips.length === 0) return undefined
  const totals = new Map<string, number>()
  for (const ip of ips) {
    if (!ip.isp) continue
    const weight = ip.hit_count > 0 ? ip.hit_count : 1
    totals.set(ip.isp, (totals.get(ip.isp) ?? 0) + weight)
  }
  if (totals.size === 0) return undefined
  return [...totals.entries()].sort((a, b) => b[1] - a[1])[0][0]
}

function dominantNetwork(report: SessionReport): { type: string; share: number } | undefined {
  const c = report.pool_composition
  const entries: Array<[string, number]> = [
    ['mobile', c.mobile], ['residential', c.residential], ['datacenter', c.datacenter], ['unknown', c.unknown],
  ]
  const sum = entries.reduce((acc, [, v]) => acc + v, 0)
  if (sum === 0) return undefined
  const [type, value] = entries.sort((a, b) => b[1] - a[1])[0]
  return { type, share: value / sum }
}

const NETWORK_ALIASES: Record<string, string> = {
  resi: 'residential', residential: 'residential', res: 'residential',
  mob: 'mobile', mobile: 'mobile', dc: 'datacenter', datacenter: 'datacenter', dch: 'datacenter',
}

function pct(share: number): string {
  return `${Math.round(share * 100)}%`
}

export function compareAttributes(parsed: ParsedAttributes, report: SessionReport | undefined, targetCountry?: string | null): ComparisonRow[] {
  const rows: ComparisonRow[] = []

  // A manually declared target country overrides any country parsed from the
  // username; it is the only requested-country signal for proxies whose config
  // does not encode one.
  const country = attribute(parsed, 'country')
  const manual = targetCountry?.trim().toUpperCase() || ''
  if (country || manual) {
    const code = manual || country!.value.toUpperCase()
    const requested = manual ? `${code} (manual)` : code
    const observed = report ? dominantCountry(report.ips) : undefined
    rows.push(observed
      ? { label: 'Country', requested, observed: `${observed.code} (${pct(observed.share)})`, verdict: observed.code.toUpperCase() === code ? (observed.share >= 0.9 ? 'match' : 'partial') : 'mismatch' }
      : { label: 'Country', requested, observed: '—', verdict: 'unknown' })
  }

  const type = attribute(parsed, 'type')
  if (type) {
    const requested = lookup(NETWORK_ALIASES, type.value.toLowerCase()) ?? type.value.toLowerCase()
    const observed = report ? dominantNetwork(report) : undefined
    rows.push(observed
      ? { label: 'Network type', requested, observed: `${observed.type} (${pct(observed.share)})`, verdict: observed.type === requested ? (observed.share >= 0.6 ? 'match' : 'partial') : 'mismatch' }
      : { label: 'Network type', requested, observed: '—', verdict: 'unknown' })
  }

  for (const key of ['region', 'state', 'city'] as const) {
    const attr = attribute(parsed, key)
    if (attr) rows.push({ label: attr.label, requested: attr.value, observed: '—', verdict: 'unknown' })
  }

  const isp = attribute(parsed, 'isp')
  if (isp) {
    const observed = report ? (dominantISP(report.ips) ?? '') : ''
    rows.push({ label: 'ISP', requested: isp.value, observed: observed || '—', verdict: observed && observed.toLowerCase().includes(isp.value.toLowerCase()) ? 'match' : observed ? 'mismatch' : 'unknown' })
  }

  return rows
}

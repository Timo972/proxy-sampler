import { useState } from 'react'
import { ChevronDown, ChevronRight, Info } from 'lucide-react'

import type { IPRow } from '../../lib/api'
import { formatTimestampWithZone } from '../../lib/format'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '../ui/tooltip'

export function IPTable({ rows }: { rows: IPRow[] }) {
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set())
  const sorted = [...rows].sort((left, right) => right.hit_count - left.hit_count || left.ip.localeCompare(right.ip))
  if (sorted.length === 0) return <p className="section-empty">IP reputation rows appear after an egress IP is observed.</p>

  const toggle = (ip: string) => setExpanded((current) => {
    const next = new Set(current)
    if (next.has(ip)) next.delete(ip)
    else next.add(ip)
    return next
  })

  return <TooltipProvider delayDuration={250}>
    <div className="ip-table-wrap">
      <table className="table ip-table">
        <thead><tr><th scope="col">IP</th><th scope="col">Category</th><th scope="col">Country</th><th scope="col">ISP</th><th scope="col">ASN</th><th scope="col">Risk</th><th scope="col">GreyNoise</th><th scope="col">DNSBL</th><th scope="col">First seen</th><th scope="col">Last seen</th><th scope="col">Hits</th></tr></thead>
        <tbody>{sorted.map((row) => <tr key={row.ip}>
          <th scope="row" className="mono">{row.ip}</th>
          <td><CategoryLabel category={row.category} /></td>
          <td>{row.country || '—'}</td>
          <td><ClippedValue label={`Full ISP for ${row.ip}`} value={row.isp || '—'} /></td>
      <td><ClippedValue label={`Full ASN for ${row.ip}`} value={row.asn || '—'} /></td>
          <td><RiskLabel score={row.risk_score} /></td>
          <td><ClippedValue label={`Full GreyNoise classification for ${row.ip}`} value={row.greynoise_class || '—'} /></td>
          <td><ClippedValue label={`DNSBL details for ${row.ip}`} value={row.dnsbl_listed ? `Listed: ${row.dnsbl_hits.join(', ') || 'unspecified list'}` : 'Not listed'} /></td>
          <td>{formatTimestampWithZone(row.first_seen)}</td>
          <td>{formatTimestampWithZone(row.last_seen)}</td>
          <td className="numeric">{row.hit_count}</td>
        </tr>)}</tbody>
      </table>
    </div>
    <div className="ip-records">
      {sorted.map((row) => {
        const open = expanded.has(row.ip)
        return <article className="ip-record" key={row.ip}>
          <div className="ip-record-summary"><strong className="mono">{row.ip}</strong><CategoryLabel category={row.category} /><RiskLabel score={row.risk_score} /></div>
          <button type="button" className="ip-details-trigger" aria-expanded={open} aria-controls={`ip-${safeID(row.ip)}-details`} onClick={() => toggle(row.ip)}>
            {open ? <ChevronDown size={16} aria-hidden="true" /> : <ChevronRight size={16} aria-hidden="true" />} {open ? 'Hide' : 'Show'} details for {row.ip}
          </button>
      {open && <dl id={`ip-${safeID(row.ip)}-details`} className="ip-details"><Detail label="Country" value={row.country || '—'} /><Detail label="ISP" value={row.isp || '—'} /><Detail label="ASN" value={row.asn || '—'} /><Detail label="GreyNoise" value={row.greynoise_class || '—'} /><Detail label="DNSBL" value={row.dnsbl_listed ? `Listed: ${row.dnsbl_hits.join(', ') || 'unspecified list'}` : 'Not listed'} /><Detail label="First seen" value={formatTimestampWithZone(row.first_seen)} /><Detail label="Last seen" value={formatTimestampWithZone(row.last_seen)} /><Detail label="Hits" value={String(row.hit_count)} /></dl>}
        </article>
      })}
    </div>
  </TooltipProvider>
}

function ClippedValue({ value, label }: { value: string; label: string }) {
  return <Tooltip><TooltipTrigger asChild><button type="button" className="clipped-value" aria-label={label}>{value}<Info size={13} aria-hidden="true" /></button></TooltipTrigger><TooltipContent side="top">{value}</TooltipContent></Tooltip>
}

function CategoryLabel({ category }: { category: string }) {
  const normalized = ['mobile', 'residential', 'datacenter'].includes(category) ? category : 'unknown'
  return <span className={`category-label category-${normalized}`}><span className="category-dot" aria-hidden="true" />{category || 'unknown'}</span>
}

function RiskLabel({ score }: { score: number | null }) {
  if (score == null) return <span className="risk-label risk-unknown">Unknown</span>
  const level = score >= 70 ? 'High' : score >= 40 ? 'Elevated' : 'Low'
  return <span className={`risk-label risk-${level.toLowerCase()}`}>{level} {score}</span>
}

function Detail({ label, value }: { label: string; value: string }) {
  return <div><dt>{label}</dt><dd>{value}</dd></div>
}

function safeID(value: string) {
  return value.replace(/[^a-zA-Z0-9_-]/g, '-')
}

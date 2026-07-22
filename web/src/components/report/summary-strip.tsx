import type { Session } from '../../lib/api'
import { formatLastIP, formatMilliseconds, formatPercent, formatTimestampWithZone } from '../../lib/format'

export function SummaryStrip({ session }: { session: Session }) {
  const metrics = [
    ['Success rate', formatPercent(session.success_rate)],
    ['Median RTT', formatMilliseconds(session.last_rtt_ms)],
    ['Distinct IPs', new Intl.NumberFormat().format(session.distinct_ips)],
    ['Samples', new Intl.NumberFormat().format(session.samples_taken)],
    ['Last IP', formatLastIP(session.last_primary_ip, session.last_primary_category)],
    ['Last sample', formatTimestampWithZone(session.last_sample_at)],
  ]
  return <dl className="summary-strip" role="region" aria-label="Session summary">
    {metrics.map(([label, value]) => <div key={label}><dt>{label}</dt><dd className={label === 'Last IP' ? 'mono' : 'numeric'}>{value}</dd></div>)}
  </dl>
}

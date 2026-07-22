import type { Session, SessionReport } from '../../lib/api'
import { compareAttributes, parseProxyAttributes } from '../../lib/proxy-attributes'

export function TargetingPanel({ session, report }: { session: Session; report: SessionReport | undefined }) {
  const parsed = parseProxyAttributes(session.proxy_username)
  if (parsed.attributes.length === 0 && parsed.raw.length === 0) {
    return (
      <section className="report-section targeting-panel">
        <h2>Targeting</h2>
        <p className="muted">No username on this proxy, so no targeting attributes are available.</p>
      </section>
    )
  }
  const rows = compareAttributes(parsed, report)
  const groups: Array<['geo' | 'session' | 'network' | 'other', string]> = [
    ['geo', 'Geo'], ['session', 'Session'], ['network', 'Network'], ['other', 'Other'],
  ]
  return (
    <section className="report-section targeting-panel">
      <h2>Targeting</h2>
      <div className="targeting-chips">
        {groups.map(([category, label]) => {
          const items = parsed.attributes.filter((a) => a.category === category)
          if (items.length === 0) return null
          return (
            <div key={category} className="targeting-group">
              <span className="targeting-group-label">{label}</span>
              {items.map((a) => (
                <span key={`${a.key}-${a.value}`} className="chip"><strong>{a.label}</strong> {a.value}</span>
              ))}
            </div>
          )
        })}
        {parsed.raw.length > 0 && (
          <div className="targeting-group">
            <span className="targeting-group-label">Unparsed</span>
            {parsed.raw.map((token, index) => <span key={`${token}-${index}`} className="chip mono">{token}</span>)}
          </div>
        )}
      </div>
      {rows.length > 0 && (
        <table className="targeting-comparison">
          <thead><tr><th>Attribute</th><th>Requested</th><th>Observed</th><th>Result</th></tr></thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.label}>
                <td>{row.label}</td>
                <td className="mono">{row.requested}</td>
                <td className="mono">{row.observed}</td>
                <td><span className={`verdict verdict-${row.verdict}`}>{row.verdict}</span></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

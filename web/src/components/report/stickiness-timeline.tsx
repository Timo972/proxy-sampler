import type { Stickiness } from '../../lib/api'
import { formatDuration, formatTimestampWithZone } from '../../lib/format'
import { ChartSection, Empty } from './timeseries-charts'

export function StickinessTimeline({ data }: { data: Stickiness }) {
  if (data.holds.length === 0) {
    return <ChartSection title="Stickiness"><Empty>Stickiness segments appear after an IP has been observed.</Empty></ChartSection>
  }

  const starts = data.holds.map((hold) => Date.parse(hold.started_at)).filter(Number.isFinite)
  const ends = data.holds.map((hold) => Date.parse(hold.ended_at)).filter(Number.isFinite)
  const start = Math.min(...starts)
  const end = Math.max(...ends)
  const span = Math.max(1, end - start)

  return (
    <ChartSection title="Stickiness">
      <p className="chart-summary">Hold segments show {data.holds.length} observed IP runs; the median hold was {formatDuration(data.median_hold_seconds)} and the average was {formatDuration(data.average_hold_seconds)}.</p>
      <div className="stickiness-track" role="img" aria-label={`${data.holds.length} separate IP hold segments, not a continuous measurement line`}>
        {data.holds.map((hold, index) => {
          const holdStart = Date.parse(hold.started_at)
          const holdEnd = Date.parse(hold.ended_at)
          const left = Number.isFinite(holdStart) ? ((holdStart - start) / span) * 100 : 0
          const width = Number.isFinite(holdEnd) && Number.isFinite(holdStart) ? Math.max(1.5, ((holdEnd - holdStart) / span) * 100) : 1.5
          return <span
            key={`${hold.ip}-${hold.started_at}-${index}`}
            className="hold-segment"
            tabIndex={0}
            style={{ left: `${Math.max(0, left)}%`, width: `${Math.min(100 - Math.max(0, left), width)}%` }}
            aria-label={`${hold.ip}, ${hold.samples} samples, held ${formatDuration(hold.duration_seconds)}, from ${formatTimestampWithZone(hold.started_at)} to ${formatTimestampWithZone(hold.ended_at)}`}
          />
        })}
      </div>
      <div className="stickiness-scale" aria-hidden="true"><span>{formatTimestampWithZone(data.holds[0]?.started_at)}</span><span>{formatTimestampWithZone(data.holds.at(-1)?.ended_at)}</span></div>
      <details className="chart-data"><summary>View stickiness data</summary><div className="chart-data-scroll"><table><caption className="sr-only">IP hold segments</caption><thead><tr><th scope="col">IP</th><th scope="col">Started</th><th scope="col">Ended</th><th scope="col">Samples</th><th scope="col">Duration</th></tr></thead><tbody>{data.holds.map((hold, index) => <tr key={`${hold.ip}-${index}`}><td className="mono">{hold.ip}</td><td>{formatTimestampWithZone(hold.started_at)}</td><td>{formatTimestampWithZone(hold.ended_at)}</td><td>{hold.samples}</td><td>{formatDuration(hold.duration_seconds)}</td></tr>)}</tbody></table></div></details>
    </ChartSection>
  )
}

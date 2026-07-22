import { Bar, BarChart, CartesianGrid, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

import type { RiskBucket } from '../../lib/api'
import { ChartSection, Empty } from './timeseries-charts'

export function RiskHistogram({ data }: { data: RiskBucket[] }) {
  const total = data.reduce((sum, bucket) => sum + bucket.count, 0)
  if (total === 0) {
    return <ChartSection title="Risk distribution"><Empty>Risk distribution appears after reputation enrichment.</Empty></ChartSection>
  }
  const chartData = data.map((bucket) => ({ ...bucket, midpoint: (bucket.min + bucket.max) / 2 }))
  return (
    <ChartSection title="Risk distribution">
      <p className="chart-summary">{total} enriched IPs are grouped by risk score. Scores at or above 70 cross the flagged threshold.</p>
      <div className="chart-frame" role="img" aria-label="IP risk-score histogram with the flag threshold marked at 70">
        <ResponsiveContainer width="100%" height={240}>
          <BarChart data={chartData} accessibilityLayer>
            <CartesianGrid stroke="var(--border)" vertical={false} />
            <XAxis type="number" dataKey="midpoint" domain={[0, 100]} ticks={[0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100]} allowDataOverflow />
            <YAxis domain={[0, 'dataMax']} allowDecimals={false} width={42} />
            <Tooltip content={<RiskTooltip />} />
            <ReferenceLine x={70} stroke="var(--danger)" strokeDasharray="4 3" label={{ value: '≥70', fill: 'var(--danger)', position: 'top' }} />
            <Bar dataKey="count" name="IPs" fill="var(--chart-datacenter)" isAnimationActive={false} />
          </BarChart>
        </ResponsiveContainer>
      </div>
      <details className="chart-data"><summary>View risk data</summary><div className="chart-data-scroll"><table><caption className="sr-only">Risk-score histogram data</caption><thead><tr><th scope="col">Risk range</th><th scope="col">Minimum</th><th scope="col">Maximum</th><th scope="col">IPs</th></tr></thead><tbody>{data.map((bucket) => <tr key={`${bucket.min}-${bucket.max}`}><td>{bucket.label}</td><td>{bucket.min}</td><td>{bucket.max}</td><td>{bucket.count}</td></tr>)}</tbody></table></div></details>
    </ChartSection>
  )
}

function RiskTooltip({ active, payload }: { active?: boolean; payload?: ReadonlyArray<{ payload?: RiskBucket }> }) {
  const bucket = payload?.[0]?.payload
  if (!active || !bucket) return null
  return <div className="chart-tooltip"><strong>Risk {bucket.label}</strong><span>{bucket.count} IPs</span></div>
}

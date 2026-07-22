import { Area, AreaChart, CartesianGrid, Cell, Pie, PieChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

import type { PoolComposition, SeriesPoint } from '../../lib/api'
import { formatTimestampWithZone } from '../../lib/format'
import { ChartSection, Empty } from './timeseries-charts'

const categories = [
  { key: 'mobile', label: 'Mobile', color: 'var(--chart-mobile)' },
  { key: 'residential', label: 'Residential', color: 'var(--chart-residential)' },
  { key: 'datacenter', label: 'Datacenter', color: 'var(--chart-datacenter)' },
  { key: 'unknown', label: 'Unknown', color: 'var(--chart-unknown)' },
] as const

export function CompositionAreaChart({ data }: { data: SeriesPoint[] }) {
  return (
    <ChartSection title="Composition over time">
      {data.length === 0 ? <Empty>Composition appears once an egress IP is classified.</Empty> : <>
        <p className="chart-summary">Each time bucket is stacked by mobile, residential, datacenter, and unknown IP observations.</p>
        <div className="chart-frame" role="img" aria-label="Stacked IP-category composition over time">
          <ResponsiveContainer width="100%" height={240}>
            <AreaChart data={data} accessibilityLayer>
              <CartesianGrid stroke="var(--border)" vertical={false} />
              <XAxis dataKey="at" tickFormatter={formatChartTime} minTickGap={28} />
              <YAxis domain={[0, 'auto']} allowDecimals={false} width={42} />
              <Tooltip content={<CompositionTooltip />} />
              {categories.map((category) => <Area key={category.key} type="monotone" dataKey={category.key} name={category.label} stackId="composition" stroke={category.color} fill={category.color} fillOpacity={0.55} isAnimationActive={false} />)}
            </AreaChart>
          </ResponsiveContainer>
        </div>
        <details className="chart-data"><summary>View composition data</summary><div className="chart-data-scroll"><table><caption className="sr-only">Composition chart data</caption><thead><tr><th scope="col">Time</th>{categories.map((category) => <th key={category.key} scope="col">{category.label}</th>)}</tr></thead><tbody>{data.map((point) => <tr key={point.at}><td>{formatTimestampWithZone(point.at)}</td>{categories.map((category) => <td key={category.key}>{point[category.key]}</td>)}</tr>)}</tbody></table></div></details>
      </>}
    </ChartSection>
  )
}

export function PoolCompositionChart({ data }: { data: PoolComposition }) {
  const parts = categories.map((category) => ({ ...category, value: data[category.key] }))
  const total = parts.reduce((sum, part) => sum + part.value, 0)
  return (
    <ChartSection title="Pool composition">
      {total === 0 ? <Empty>Pool composition appears once an egress IP is classified.</Empty> : <div className="donut-layout">
        <div className="donut-chart" role="img" aria-label={`Pool composition across ${total} classified IPs`}>
          <ResponsiveContainer width="100%" height={240}>
            <PieChart accessibilityLayer>
              <Pie data={parts} dataKey="value" nameKey="label" innerRadius="55%" outerRadius="82%" paddingAngle={1} isAnimationActive={false}>
                {parts.map((part) => <Cell key={part.key} fill={part.color} />)}
              </Pie>
              <Tooltip content={<PoolTooltip />} />
            </PieChart>
          </ResponsiveContainer>
        </div>
        <dl className="composition-legend">
          {parts.map((part) => <div key={part.key}><dt><span className="legend-swatch" style={{ background: part.color }} />{part.label}</dt><dd className="numeric">{part.value}</dd></div>)}
        </dl>
      </div>}
    </ChartSection>
  )
}

interface Payload { name?: string; value?: number; color?: string; payload?: { label?: string; color?: string } }

function CompositionTooltip({ active, label, payload }: { active?: boolean; label?: string; payload?: readonly Payload[] }) {
  if (!active || !payload?.length) return null
  return <div className="chart-tooltip"><strong>{formatTimestampWithZone(label)}</strong>{payload.map((entry) => <span key={entry.name} style={{ color: entry.color }}>{entry.name}: {entry.value}</span>)}</div>
}

function PoolTooltip({ active, payload }: { active?: boolean; payload?: readonly Payload[] }) {
  if (!active || !payload?.length) return null
  const entry = payload[0]
  return <div className="chart-tooltip"><strong>{entry.payload?.label ?? entry.name}</strong><span>{entry.value ?? 0} IPs</span></div>
}

function formatChartTime(value: string) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(date)
}

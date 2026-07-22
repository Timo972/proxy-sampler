import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

import type { PoolGrowthPoint, SeriesPoint } from '../../lib/api'
import { formatMilliseconds, formatPercent, formatTimestampWithZone } from '../../lib/format'

export const SUCCESS_DOMAIN: [number, number] = [0, 1]

export function SuccessRateChart({ data }: { data: SeriesPoint[] }) {
  return (
    <ChartSection title="Success rate">
      {data.length === 0 ? <Empty>Success data will appear after the first completed sample.</Empty> : <>
        <p className="chart-summary">Success ranged from {formatPercent(Math.min(...data.map((point) => point.success_rate)))} to {formatPercent(Math.max(...data.map((point) => point.success_rate)))} across {data.length} time buckets.</p>
        <div className="chart-frame" role="img" aria-label="Success rate over time, scaled from zero to one hundred percent">
          <ResponsiveContainer width="100%" height={240}>
            <LineChart data={data} accessibilityLayer>
              <CartesianGrid stroke="var(--border)" vertical={false} />
              <XAxis dataKey="at" tickFormatter={formatChartTime} minTickGap={28} />
              <YAxis domain={SUCCESS_DOMAIN} ticks={[0, 0.25, 0.5, 0.75, 1]} tickFormatter={(value: number) => formatPercent(value)} width={54} />
              <Tooltip content={<ChartTooltip valueFormatter={(value) => formatPercent(value)} />} />
              <Line type="monotone" dataKey="success_rate" name="Success rate" stroke="var(--primary)" strokeWidth={2} dot={false} isAnimationActive={false} />
            </LineChart>
          </ResponsiveContainer>
        </div>
        <SeriesDataTable caption="Success-rate chart data" data={data} columns={[['Success rate', (point) => formatPercent(point.success_rate)]]} />
      </>}
    </ChartSection>
  )
}

export function LatencyChart({ data }: { data: SeriesPoint[] }) {
  const measured = data.filter((point) => point.success_rate > 0)
  return (
    <ChartSection title="Latency">
      {measured.length === 0 ? <Empty>Latency data will appear after a successful probe.</Empty> : <>
        <p className="chart-summary">Latest median latency was {formatMilliseconds(measured.at(-1)?.latency_p50_ms)}; latest p95 was {formatMilliseconds(measured.at(-1)?.latency_p95_ms)}.</p>
        <div className="chart-frame" role="img" aria-label="Median and p95 latency in milliseconds over time">
          <ResponsiveContainer width="100%" height={240}>
            <LineChart data={measured} accessibilityLayer>
              <CartesianGrid stroke="var(--border)" vertical={false} />
              <XAxis dataKey="at" tickFormatter={formatChartTime} minTickGap={28} />
              <YAxis domain={[0, 'auto']} tickFormatter={(value: number) => `${value} ms`} width={62} />
              <Tooltip content={<ChartTooltip valueFormatter={(value) => formatMilliseconds(value)} />} />
              <Line type="monotone" dataKey="latency_p50_ms" name="p50" stroke="var(--chart-residential)" strokeWidth={2} dot={false} isAnimationActive={false} />
              <Line type="monotone" dataKey="latency_p95_ms" name="p95" stroke="var(--chart-datacenter)" strokeWidth={2} strokeDasharray="5 4" dot={false} isAnimationActive={false} />
            </LineChart>
          </ResponsiveContainer>
        </div>
        <SeriesDataTable caption="Latency chart data" data={measured} columns={[
          ['p50', (point) => formatMilliseconds(point.latency_p50_ms)],
          ['p95', (point) => formatMilliseconds(point.latency_p95_ms)],
        ]} />
      </>}
    </ChartSection>
  )
}

export function PoolGrowthChart({ data }: { data: PoolGrowthPoint[] }) {
  return (
    <ChartSection title="Pool growth">
      {data.length === 0 ? <Empty>Pool growth appears after an egress IP is observed.</Empty> : <>
        <p className="chart-summary">The observed pool grew to {data.at(-1)?.distinct_ips ?? 0} distinct IPs across {data.length} time buckets.</p>
        <div className="chart-frame" role="img" aria-label="Cumulative distinct egress IPs over time">
          <ResponsiveContainer width="100%" height={240}>
            <LineChart data={data} accessibilityLayer>
              <CartesianGrid stroke="var(--border)" vertical={false} />
              <XAxis dataKey="at" tickFormatter={formatChartTime} minTickGap={28} />
              <YAxis domain={[0, 'dataMax']} allowDecimals={false} width={42} />
              <Tooltip content={<ChartTooltip valueFormatter={(value) => new Intl.NumberFormat().format(value)} />} />
              <Line type="stepAfter" dataKey="distinct_ips" name="Distinct IPs" stroke="var(--primary)" strokeWidth={2} dot={false} isAnimationActive={false} />
            </LineChart>
          </ResponsiveContainer>
        </div>
        <details className="chart-data">
          <summary>View pool-growth data</summary>
          <div className="chart-data-scroll"><table><caption className="sr-only">Pool-growth chart data</caption><thead><tr><th scope="col">Time</th><th scope="col">Distinct IPs</th></tr></thead><tbody>{data.map((point) => <tr key={point.at}><td>{formatTimestampWithZone(point.at)}</td><td>{point.distinct_ips}</td></tr>)}</tbody></table></div>
        </details>
      </>}
    </ChartSection>
  )
}

export function ChartSection({ title, children }: { title: string; children: React.ReactNode }) {
  return <section className="report-section"><h2>{title}</h2>{children}</section>
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <p className="section-empty">{children}</p>
}

interface TooltipPayload {
  color?: string
  name?: string
  value?: number | string
}

function ChartTooltip({ active, label, payload, valueFormatter }: {
  active?: boolean
  label?: string | number
  payload?: readonly TooltipPayload[]
  valueFormatter: (value: number) => string
}) {
  if (!active || !payload?.length) return null
  return <div className="chart-tooltip"><strong>{formatTimestampWithZone(String(label))}</strong>{payload.map((entry) => <span key={entry.name} style={{ color: entry.color }}>{entry.name}: {valueFormatter(Number(entry.value))}</span>)}</div>
}

function SeriesDataTable({ data, caption, columns }: {
  data: SeriesPoint[]
  caption: string
  columns: Array<[string, (point: SeriesPoint) => string]>
}) {
  return (
    <details className="chart-data">
      <summary>View chart data</summary>
      <div className="chart-data-scroll"><table><caption className="sr-only">{caption}</caption><thead><tr><th scope="col">Time</th>{columns.map(([label]) => <th scope="col" key={label}>{label}</th>)}</tr></thead><tbody>{data.map((point) => <tr key={point.at}><td>{formatTimestampWithZone(point.at)}</td>{columns.map(([label, read]) => <td key={label}>{read(point)}</td>)}</tr>)}</tbody></table></div>
    </details>
  )
}

function formatChartTime(value: string) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(date)
}

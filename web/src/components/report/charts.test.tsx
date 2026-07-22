import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { PoolComposition, PoolGrowthPoint, RiskBucket, SeriesPoint } from '../../lib/api'
import { formatTimestampWithZone } from '../../lib/format'
import { CompositionAreaChart, PoolCompositionChart } from './composition-charts'
import { RiskHistogram } from './risk-histogram'
import { LatencyChart, PoolGrowthChart, SuccessRateChart } from './timeseries-charts'

vi.mock('recharts', async () => {
  const { cloneElement, isValidElement } = await import('react')
  const container = (kind: string) => ({ children, accessibilityLayer }: { children?: React.ReactNode; accessibilityLayer?: boolean }) => (
    <div data-recharts={kind} data-accessibility-layer={String(Boolean(accessibilityLayer))}>{children}</div>
  )
  const primitive = (kind: string) => (props: Record<string, unknown>) => (
    <div
      data-recharts={kind}
      data-data-key={String(props.dataKey ?? '')}
      data-domain={JSON.stringify(props.domain ?? null)}
      data-stroke={String(props.stroke ?? '')}
      data-fill={String(props.fill ?? '')}
      data-animation={String(props.isAnimationActive ?? '')}
      data-reference={typeof props.label === 'object' && props.label ? String((props.label as { value?: string }).value ?? '') : ''}
      data-ticks={Array.isArray(props.ticks) && typeof props.tickFormatter === 'function'
        ? JSON.stringify(props.ticks.map((tick) => (props.tickFormatter as (value: number) => string)(tick)))
        : ''}
    />
  )
  const Tooltip = ({ content }: { content?: React.ReactNode }) => {
    if (!isValidElement(content)) return null
    return <div data-recharts="Tooltip">{cloneElement(content, {
      active: true,
      label: '2026-07-21T12:00:00Z',
      payload: [{
        color: 'var(--primary)',
        name: 'Value',
        value: 1,
        payload: { label: '70–100', min: 70, max: 100, count: 1 },
      }],
    } as Record<string, unknown>)}</div>
  }
  return {
    Area: primitive('Area'),
    AreaChart: container('AreaChart'),
    Bar: primitive('Bar'),
    BarChart: container('BarChart'),
    CartesianGrid: primitive('CartesianGrid'),
    Cell: primitive('Cell'),
    Line: primitive('Line'),
    LineChart: container('LineChart'),
    Pie: ({ children, ...props }: { children?: React.ReactNode } & Record<string, unknown>) => <div data-recharts="Pie" data-animation={String(props.isAnimationActive ?? '')}>{children}</div>,
    PieChart: container('PieChart'),
    ReferenceLine: primitive('ReferenceLine'),
    ResponsiveContainer: ({ children }: { children?: React.ReactNode }) => <div data-recharts="ResponsiveContainer">{children}</div>,
    Tooltip,
    XAxis: primitive('XAxis'),
    YAxis: primitive('YAxis'),
  }
})

const series: SeriesPoint[] = [
  { at: '2026-07-21T12:00:00Z', success_rate: 0.25, latency_p50_ms: 120, latency_p95_ms: 190, distinct_per_sample: 1, ip_changes: 0, mobile: 1, residential: 2, datacenter: 3, unknown: 4 },
  { at: '2026-07-21T12:05:00Z', success_rate: 1, latency_p50_ms: 140, latency_p95_ms: 230, distinct_per_sample: 2, ip_changes: 1, mobile: 2, residential: 3, datacenter: 4, unknown: 5 },
]
const growth: PoolGrowthPoint[] = [
  { at: series[0].at, distinct_ips: 1 },
  { at: series[1].at, distinct_ips: 3 },
]
const composition: PoolComposition = { mobile: 1, residential: 2, datacenter: 3, unknown: 4 }
const risk: RiskBucket[] = [{ label: '70–100', min: 70, max: 100, count: 1 }]

describe('report chart contracts', () => {
  it('uses truthful fixed scales, stable tokens, responsive containers, and static chart marks', () => {
    const { container } = render(<>
      <SuccessRateChart data={series} />
      <LatencyChart data={series} />
      <CompositionAreaChart data={series} />
      <PoolCompositionChart data={composition} />
      <PoolGrowthChart data={growth} />
      <RiskHistogram data={risk} />
    </>)

    expect(container.querySelectorAll('[data-recharts="ResponsiveContainer"]')).toHaveLength(6)
    const yAxes = [...container.querySelectorAll('[data-recharts="YAxis"]')]
    expect(yAxes[0]).toHaveAttribute('data-domain', '[0,1]')
    expect(yAxes[0]).toHaveAttribute('data-ticks', '["0%","25%","50%","75%","100%"]')
    expect(yAxes[1]).toHaveAttribute('data-domain', '[0,"auto"]')
    expect(yAxes[2]).toHaveAttribute('data-domain', '[0,"auto"]')
    expect(yAxes[3]).toHaveAttribute('data-domain', '[0,"dataMax"]')
    expect(yAxes[4]).toHaveAttribute('data-domain', '[0,"dataMax"]')

    expect([...container.querySelectorAll('[data-recharts="Area"]')].map((node) => node.getAttribute('data-stroke'))).toEqual([
      'var(--chart-mobile)',
      'var(--chart-residential)',
      'var(--chart-datacenter)',
      'var(--chart-unknown)',
    ])
    expect([...container.querySelectorAll('[data-recharts="Cell"]')].map((node) => node.getAttribute('data-fill'))).toEqual([
      'var(--chart-mobile)',
      'var(--chart-residential)',
      'var(--chart-datacenter)',
      'var(--chart-unknown)',
    ])
    expect(container.querySelector('[data-recharts="ReferenceLine"]')).toHaveAttribute('data-reference', '≥70')
    for (const mark of container.querySelectorAll('[data-recharts="Line"], [data-recharts="Area"], [data-recharts="Pie"], [data-recharts="Bar"]')) {
      expect(mark).toHaveAttribute('data-animation', 'false')
    }
  })

  it('keeps local-zone tooltip timestamps and keyboard-readable text companions beside every chart', () => {
    render(<>
      <SuccessRateChart data={series} />
      <LatencyChart data={series} />
      <CompositionAreaChart data={series} />
      <PoolCompositionChart data={composition} />
      <PoolGrowthChart data={growth} />
      <RiskHistogram data={risk} />
    </>)

    expect(screen.getAllByText(formatTimestampWithZone(series[0].at)).length).toBeGreaterThanOrEqual(4)
    for (const name of [
      'Success-rate chart data',
      'Latency chart data',
      'Composition chart data',
      'Pool-growth chart data',
      'Risk-score histogram data',
    ]) {
      expect(screen.getByText(name)).toBeInTheDocument()
    }
  expect(document.querySelector('.composition-legend')).toHaveTextContent('Mobile')
  })
})

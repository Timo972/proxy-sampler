import { Fragment, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { useSearchParams } from 'react-router-dom'

import type { SampleEvent, SamplePage } from '../../lib/api'
import { formatMilliseconds, formatTimestampWithZone } from '../../lib/format'
import { Button } from '../ui/button'

export function GroupedSamplesTable({ page }: { page: SamplePage }) {
  const [expanded, setExpanded] = useState<Set<number>>(() => new Set())
  const [searchParams, setSearchParams] = useSearchParams()
  const currentURLPage = positiveInteger(searchParams.get('page'), page.page)
  const items = [...page.items].sort((left, right) => {
    const byTime = Date.parse(right.sampled_at) - Date.parse(left.sampled_at)
    return byTime || right.sample_seq - left.sample_seq
  })
  const pageCount = Math.max(1, Math.ceil(page.total / page.page_size))

  if (items.length === 0) {
    return <p className="section-empty">Samples will appear after the first sampling interval completes.</p>
  }

  const toggle = (sampleSeq: number) => {
    setExpanded((current) => {
      const next = new Set(current)
      if (next.has(sampleSeq)) next.delete(sampleSeq)
      else next.add(sampleSeq)
      return next
    })
  }

  const changePage = (nextPage: number) => {
    const next = new URLSearchParams(searchParams)
    next.set('tab', 'samples')
    next.set('page', String(nextPage))
    setSearchParams(next)
  }

  return (
    <div className="sample-table-section">
      <div className="sample-table-wrap">
        <table className="table sample-table">
          <thead>
            <tr>
              <th scope="col">Sample</th>
              <th scope="col">Sampled at</th>
              <th scope="col">Probes</th>
              <th scope="col">Primary IP</th>
              <th scope="col">Distinct IPs</th>
              <th scope="col">Changed</th>
              <th scope="col">New IPs</th>
              <th scope="col">RTT min / med / max</th>
              <th scope="col">Category</th>
              <th scope="col">Risk</th>
        <th scope="col">Error</th>
            </tr>
          </thead>
          {items.map((sample) => (
            <SampleRows
              key={sample.sample_seq}
              sample={sample}
              open={expanded.has(sample.sample_seq)}
              onToggle={() => toggle(sample.sample_seq)}
            />
          ))}
        </table>
      </div>
      <nav className="pagination" aria-label="Sample pages">
        <Button
          type="button"
          variant="secondary"
          size="small"
          disabled={currentURLPage <= 1}
          aria-label="Previous page"
          onClick={() => changePage(currentURLPage - 1)}
        >
          Previous
        </Button>
        <span className="numeric">Page {page.page} of {pageCount}</span>
        <Button
          type="button"
          variant="secondary"
          size="small"
          disabled={currentURLPage >= pageCount}
          aria-label="Next page"
          onClick={() => changePage(currentURLPage + 1)}
        >
          Next
        </Button>
      </nav>
    </div>
  )
}

function SampleRows({ sample, open, onToggle }: { sample: SampleEvent; open: boolean; onToggle: () => void }) {
  return (
    <Fragment>
      <tbody className="sample-group">
        <tr className="sample-parent-row">
          <th scope="row">
            <button
              type="button"
              className="sample-disclosure"
              aria-label={`Sample ${sample.sample_seq} details`}
              aria-expanded={open}
              aria-controls={`sample-${sample.sample_seq}-probes`}
              onClick={onToggle}
            >
              {open ? <ChevronDown size={16} aria-hidden="true" /> : <ChevronRight size={16} aria-hidden="true" />}
              <span>Sample {sample.sample_seq}</span>
            </button>
          </th>
          <td title={formatTimestampWithZone(sample.sampled_at)}>{formatTimestampWithZone(sample.sampled_at)}</td>
          <td className="numeric">{sample.probes_ok} / {sample.probes_attempted}</td>
          <td className="mono">{sample.primary_ip ?? '—'}</td>
          <td className="numeric">{sample.distinct_ips}</td>
      <td><span className={`badge ${sample.ip_changed ? 'sample-change' : 'sample-stable'}`}>{sample.ip_changed ? 'Changed' : 'Stable'}</span></td>
          <td className="numeric">{sample.new_ips}</td>
          <td className="numeric">{formatMilliseconds(sample.rtt_min_ms)} / {formatMilliseconds(sample.rtt_med_ms)} / {formatMilliseconds(sample.rtt_max_ms)}</td>
          <td className="capitalize">{sample.primary_category || 'unknown'}</td>
          <td className="numeric">{sample.primary_risk}</td>
      <td className="compact-cell" title={sample.error || undefined}>{sample.error || '—'}</td>
        </tr>
        {open && Array.from({ length: Math.max(0, sample.probes_attempted) }, (_, index) => (
          <ProbeRow key={index} sample={sample} index={index} />
        ))}
      </tbody>
    </Fragment>
  )
}

function ProbeRow({ sample, index }: { sample: SampleEvent; index: number }) {
  const success = sample.probe_ok[index] ?? false
  const ip = sample.probe_ips[index] ?? '—'
  const rtt = sample.probe_rtts_ms[index]
  return (
    <tr id={index === 0 ? `sample-${sample.sample_seq}-probes` : undefined} className="probe-row" aria-label={`Probe ${index} for sample ${sample.sample_seq}`}>
      <th scope="row">Probe {index}</th>
      <td>{success ? 'Succeeded' : 'Failed'}</td>
      <td className="mono">{ip}</td>
      <td className="numeric">{formatMilliseconds(success && Number.isFinite(rtt) ? rtt : null)}</td>
    <td colSpan={7}>{success ? '—' : sample.error || 'Probe did not complete.'}</td>
    </tr>
  )
}

function positiveInteger(value: string | null, fallback: number) {
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : fallback
}

import { AlertTriangle, Download, RotateCcw, Square, Trash2 } from 'lucide-react'
import { Link, useNavigate, useParams } from 'react-router-dom'

import { IPTable } from '../components/report/ip-table'
import { PoolCompositionChart } from '../components/report/composition-charts'
import { RiskHistogram } from '../components/report/risk-histogram'
import { LatencyChart, SuccessRateChart } from '../components/report/timeseries-charts'
import { SessionStatusBadge } from '../components/session-status-badge'
import { EditableTitle } from '../components/editable-title'
import { Alert } from '../components/ui/alert'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table'
import {
  APIError,
  type CellReport,
  type PoolComposition,
  type Run,
  type RunReport,
  type VariantSummary,
  useDeleteRun,
  useEditRun,
  useReenableRun,
  useRun,
  useRunReport,
  useStopRun,
} from '../lib/api'
import { formatPercent, formatTimestampWithZone } from '../lib/format'

export function RunPage() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const run = useRun(id)
  const running = run.data?.run.status === 'running'
  const report = useRunReport(id, running)
  const stop = useStopRun()
  const reenable = useReenableRun()
  const del = useDeleteRun()
  const edit = useEditRun()

  if (run.isPending) return <RunLoading />
  if (run.error instanceof APIError && run.error.status === 404) return <NotFound />
  if (run.isError || !run.data) {
    return (
      <main className="session-page">
        <Alert className="error-state">
          <AlertTriangle aria-hidden="true" />
          <div><strong>Could not load run</strong><p>{errorMessage(run.error)}</p></div>
          <Button type="button" onClick={() => run.refetch()}>Retry</Button>
        </Alert>
        <Link to="/" className="back-link">Back to runs</Link>
      </main>
    )
  }

  const { run: detail, variants } = run.data
  // Every child of a run shares one manual target country, set at creation.
  const targetCountry = variants.find((v) => v.target_country)?.target_country

  const confirmStop = () => {
    if (window.confirm(`Stop “${detail.name}”? You can re-enable it later.`)) stop.mutate(id)
  }
  const confirmReenable = () => {
    if (window.confirm(`Re-enable “${detail.name}”? Sampling resumes across all variants; progress counters reset while prior samples are kept.`)) reenable.mutate(id)
  }
  const confirmDelete = () => {
    if (window.confirm(`Delete “${detail.name}”? This removes the run and all its variant sessions. This cannot be undone.`)) {
      del.mutate(id, { onSuccess: () => navigate('/') })
    }
  }

  return (
    <main className="session-page">
      <Link to="/" className="breadcrumb">Runs</Link>
      <header className="session-header">
        <div className="session-title">
          <div className="session-title-line"><EditableTitle value={detail.name} label="run name" pending={edit.isPending} onSave={(name) => edit.mutateAsync({ id, name })} /><SessionStatusBadge status={detail.status} /></div>
          <p><span className="mono">{detail.template_display}</span><span aria-hidden="true"> · </span><span>{detail.variant_count} variant{detail.variant_count === 1 ? '' : 's'}</span>{targetCountry && <><span aria-hidden="true"> · </span><span>Target country <span className="mono">{targetCountry}</span> (manual)</span></>}</p>
        </div>
        <div className="session-actions">
          {detail.status === 'running' && <Button type="button" variant="danger" onClick={confirmStop} disabled={stop.isPending}><Square size={14} fill="currentColor" aria-hidden="true" />{stop.isPending ? 'Stopping…' : 'Stop run'}</Button>}
          {detail.status !== 'running' && <Button type="button" onClick={confirmReenable} disabled={reenable.isPending}><RotateCcw size={14} aria-hidden="true" />{reenable.isPending ? 'Re-enabling…' : 'Re-enable'}</Button>}
          <Button type="button" variant="secondary" onClick={confirmDelete} disabled={del.isPending}><Trash2 size={14} aria-hidden="true" />{del.isPending ? 'Deleting…' : 'Delete'}</Button>
          <a className="button button-secondary" href={`/api/runs/${id}/export.csv`} download><Download size={16} aria-hidden="true" />Export pool CSV</a>
        </div>
      </header>
      {stop.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not stop run</strong><p>{errorMessage(stop.error)}</p></div></Alert>}
      {reenable.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not re-enable run</strong><p>{errorMessage(reenable.error)}</p></div></Alert>}
      {del.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not delete run</strong><p>{errorMessage(del.error)}</p></div></Alert>}
      {edit.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not rename run</strong><p>{errorMessage(edit.error)}</p></div></Alert>}

      <ReportBoundary report={report}>
        {report.data && <>
          <RunSummaryStrip run={detail} report={report.data} />
          <div className="report-meta" aria-live="polite">{report.dataUpdatedAt > 0 ? `Last updated ${formatTimestampWithZone(new Date(report.dataUpdatedAt).toISOString())}` : 'Waiting for report update'}</div>
          <div className="report-grid">
            <SuccessRateChart data={report.data.series} />
            <LatencyChart data={report.data.series} />
            <PoolCompositionChart data={report.data.composition} />
            <RiskHistogram data={report.data.risk_histogram} />
          </div>
          <section className="report-section"><h2>Per-cell breakdown</h2><CellTable cells={report.data.cells} /></section>
          <section className="report-section"><h2>IP reputation</h2><IPTable rows={report.data.ips} /></section>
        </>}
      </ReportBoundary>

      <section className="report-section"><h2>Variants</h2><VariantsTable variants={variants} /></section>
    </main>
  )
}

function RunSummaryStrip({ run, report }: { run: Run; report: RunReport }) {
  const poolSize = new Intl.NumberFormat().format(report.estimated_pool_size)
  const metrics: Array<[string, React.ReactNode]> = [
    ['Variants', new Intl.NumberFormat().format(run.variant_count)],
    ['Distinct IPs', new Intl.NumberFormat().format(report.distinct_ips)],
    ['Estimated pool size', report.pool_size_lower_bound
      ? <abbr title="Lower-bound estimate: sampling may not have covered the full pool yet.">{`≥ ${poolSize}`}</abbr>
      : poolSize],
    ['Honor rate', report.honor_rate == null ? '—' : formatPercent(report.honor_rate)],
    ['Flagged', formatPercent(report.flagged_percent / 100)],
    ['DNSBL hits', new Intl.NumberFormat().format(report.dnsbl_hit_ips)],
  ]
  return (
    <dl className="summary-strip" role="region" aria-label="Run summary">
      {metrics.map(([label, value]) => <div key={label}><dt>{label}</dt><dd className="numeric">{value}</dd></div>)}
    </dl>
  )
}

function CellTable({ cells }: { cells: CellReport[] }) {
  if (cells.length === 0) return <p className="section-empty">Per-cell data appears once a variant reports samples.</p>
  const sorted = [...cells].sort((left, right) => right.variant_count - left.variant_count || left.cell_key.localeCompare(right.cell_key))
  return (
    <div className="data-table-wrap">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Cell</TableHead>
            <TableHead>Variants</TableHead>
            <TableHead>Distinct IPs</TableHead>
            <TableHead>Honor rate</TableHead>
            <TableHead>Composition</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {sorted.map((cell) => (
            <TableRow key={cell.cell_key}>
              <TableCell className="mono compact-cell">{formatParams(cell.params)}{cell.target_country && <span className="muted"> · target {cell.target_country} (manual)</span>}</TableCell>
              <TableCell className="numeric">{cell.variant_count}</TableCell>
              <TableCell className="numeric">{cell.distinct_ips}</TableCell>
              <TableCell className="numeric">{cell.honor_rate == null ? '—' : formatPercent(cell.honor_rate)}</TableCell>
              <TableCell>{formatComposition(cell.composition)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function VariantsTable({ variants }: { variants: VariantSummary[] }) {
  const navigate = useNavigate()
  if (variants.length === 0) return <p className="section-empty">Variants will appear once the run expands into sessions.</p>
  return (
    <div className="data-table-wrap">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Cell</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Samples</TableHead>
            <TableHead>Distinct IPs</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {variants.map((variant) => (
            <TableRow key={variant.session_id} className="session-row" onClick={() => navigate(`/sessions/${variant.session_id}`)}>
              <TableCell><Link className="session-link" to={`/sessions/${variant.session_id}`} onClick={(event) => event.stopPropagation()}><strong>{variant.name}</strong></Link></TableCell>
              <TableCell className="mono compact-cell">{formatParams(variant.params)}</TableCell>
              <TableCell><SessionStatusBadge status={variant.status} /></TableCell>
              <TableCell className="numeric">{variant.samples_taken}</TableCell>
              <TableCell className="numeric">{variant.distinct_ips}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function formatParams(params: Record<string, string>): string {
  const entries = Object.entries(params)
  return entries.length === 0 ? '—' : entries.map(([key, value]) => `${key}=${value}`).join(', ')
}

function formatComposition(composition: PoolComposition): string {
  const parts: string[] = []
  if (composition.mobile) parts.push(`${composition.mobile} mobile`)
  if (composition.residential) parts.push(`${composition.residential} residential`)
  if (composition.datacenter) parts.push(`${composition.datacenter} datacenter`)
  if (composition.unknown) parts.push(`${composition.unknown} unknown`)
  return parts.length === 0 ? '—' : parts.join(' · ')
}

function ReportBoundary({ report, children }: { report: ReturnType<typeof useRunReport>; children: React.ReactNode }) {
  if (report.isPending) return <ReportLoading />
  if (report.isError) return <Alert><AlertTriangle aria-hidden="true" /><div><strong>Could not load report</strong><p>{errorMessage(report.error)}</p></div><Button type="button" onClick={() => report.refetch()}>Retry report</Button></Alert>
  return children
}

function RunLoading() {
  return (
    <main className="session-page session-loading">
      <div role="status" aria-label="Loading run detail">
        <span className="sr-only">Loading run detail</span>
        <Skeleton className="session-heading-skeleton" />
        <Skeleton className="session-subheading-skeleton" />
      </div>
      <ReportLoading />
    </main>
  )
}

function ReportLoading() {
  return <div className="report-loading" aria-label="Loading report"><Skeleton className="summary-skeleton" />{Array.from({ length: 4 }, (_, index) => <Skeleton key={index} className="chart-skeleton" />)}</div>
}

function NotFound() {
  return <main className="empty-state"><h1>Run not found</h1><p>This run may have been removed, or the link may be incomplete.</p><Link to="/" className="button button-primary">Back to runs</Link></main>
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : 'The request could not be completed.'
}

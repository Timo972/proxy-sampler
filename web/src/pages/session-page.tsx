import { AlertTriangle, Download, RotateCcw, Square } from 'lucide-react'
import { Link, useParams, useSearchParams } from 'react-router-dom'

import { GroupedSamplesTable } from '../components/report/grouped-samples-table'
import { IPTable } from '../components/report/ip-table'
import { CompositionAreaChart, PoolCompositionChart } from '../components/report/composition-charts'
import { RiskHistogram } from '../components/report/risk-histogram'
import { StickinessTimeline } from '../components/report/stickiness-timeline'
import { SummaryStrip } from '../components/report/summary-strip'
import { TargetingPanel } from '../components/report/targeting-panel'
import { LatencyChart, PoolGrowthChart, SuccessRateChart } from '../components/report/timeseries-charts'
import { SessionStatusBadge } from '../components/session-status-badge'
import { EditableTitle } from '../components/editable-title'
import { Alert } from '../components/ui/alert'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '../components/ui/tabs'
import { APIError, useEditSession, useReenableSession, useSession, useSessionReport, useSessionSamples, useStopSession } from '../lib/api'
import { formatPercent, formatTimestampWithZone } from '../lib/format'

type ReportTab = 'overview' | 'reputation' | 'samples'

export function SessionPage() {
  const { id = '' } = useParams()
  const [searchParams, setSearchParams] = useSearchParams()
  const requestedTab = searchParams.get('tab')
  const tab: ReportTab = requestedTab === 'reputation' || requestedTab === 'samples' ? requestedTab : 'overview'
  const page = positiveInteger(searchParams.get('page'))
  const session = useSession(id)
  const running = session.data?.status === 'running'
  const report = useSessionReport(id, running)
  const samples = useSessionSamples(id, page, tab === 'samples', running)
  const stop = useStopSession()
  const reenable = useReenableSession()
  const edit = useEditSession()

  if (session.isPending) return <SessionLoading includeSamples={tab === 'samples'} />
  if (session.error instanceof APIError && session.error.status === 404) return <NotFound />
  if (session.isError || !session.data) {
    return <div className="session-page"><Alert className="error-state"><AlertTriangle aria-hidden="true" /><div><strong>Could not load session</strong><p>{errorMessage(session.error)}</p></div><Button type="button" onClick={() => session.refetch()}>Retry</Button></Alert><Link to="/" className="back-link">Back to sessions</Link></div>
  }

  const changeTab = (value: string) => {
    if (value !== 'overview' && value !== 'reputation' && value !== 'samples') return
    const next = new URLSearchParams(searchParams)
    next.set('tab', value)
    if (value === 'samples') {
      if (!next.has('page')) next.set('page', '1')
    } else {
      next.delete('page')
    }
    setSearchParams(next)
  }

  const confirmStop = () => {
    if (window.confirm(`Stop “${session.data.name}”? You can re-enable it later.`)) stop.mutate(id)
  }
  const confirmReenable = () => {
    if (window.confirm(`Re-enable “${session.data.name}”? Sampling resumes; progress counters reset while prior samples are kept.`)) reenable.mutate(id)
  }

  return (
    <main className="session-page">
      <Link to="/" className="breadcrumb">Sessions</Link>
      <header className="session-header">
        <div className="session-title">
          <div className="session-title-line"><EditableTitle value={session.data.name} label="session name" pending={edit.isPending} onSave={(name) => edit.mutateAsync({ id, name })} /><SessionStatusBadge status={session.data.status} /></div>
          <p><span className="mono">{session.data.proxy_display}</span><span aria-hidden="true"> · </span><span>{session.data.mode === 'sticky' ? 'Sticky' : 'Pool'}</span></p>
        </div>
        <div className="session-actions">
          {session.data.status === 'running' && <Button type="button" variant="danger" onClick={confirmStop} disabled={stop.isPending}><Square size={14} fill="currentColor" aria-hidden="true" />{stop.isPending ? 'Stopping…' : 'Stop session'}</Button>}
          {session.data.status !== 'running' && <Button type="button" onClick={confirmReenable} disabled={reenable.isPending}><RotateCcw size={14} aria-hidden="true" />{reenable.isPending ? 'Re-enabling…' : 'Re-enable'}</Button>}
          <a className="button button-secondary" href={`/api/sessions/${id}/export.csv`} download><Download size={16} aria-hidden="true" />Export CSV</a>
        </div>
      </header>
      {stop.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not stop session</strong><p>{errorMessage(stop.error)}</p></div></Alert>}
      {reenable.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not re-enable session</strong><p>{errorMessage(reenable.error)}</p></div></Alert>}
      {edit.isError && <Alert className="inline-alert"><AlertTriangle aria-hidden="true" /><div><strong>Could not rename session</strong><p>{errorMessage(edit.error)}</p></div></Alert>}
      <SummaryStrip session={session.data} />
      <div className="report-meta" aria-live="polite">{report.dataUpdatedAt > 0 ? `Last updated ${formatTimestampWithZone(new Date(report.dataUpdatedAt).toISOString())}` : 'Waiting for report update'}</div>

      <Tabs className="report-tabs" value={tab} onValueChange={changeTab}>
        <TabsList className="tabs-list" aria-label="Session report views">
          <TabsTrigger className="tab-trigger" value="overview">Overview</TabsTrigger>
          <TabsTrigger className="tab-trigger" value="reputation">Reputation</TabsTrigger>
          <TabsTrigger className="tab-trigger" value="samples">Samples</TabsTrigger>
        </TabsList>
        <TabsContent className="tab-content" value="overview">
          <TargetingPanel session={session.data} report={report.data} />
          <ReportBoundary report={report}>
            {report.data && <div className="report-grid">
              <SuccessRateChart data={report.data.series} />
              <LatencyChart data={report.data.series} />
              <CompositionAreaChart data={report.data.series} />
              <PoolCompositionChart data={report.data.pool_composition} />
              <StickinessTimeline data={report.data.stickiness} />
              <PoolGrowthChart data={report.data.pool_growth} />
            </div>}
          </ReportBoundary>
        </TabsContent>
        <TabsContent className="tab-content" value="reputation">
          <ReportBoundary report={report}>
            {report.data && <>
              <div className="reputation-summary" aria-label="Reputation summary">
                <span><strong>{report.data.reputation_summary.total_ips}</strong> observed IPs</span>
                <span><strong>{report.data.reputation_summary.flagged_ips}</strong> flagged ({formatPercent(report.data.reputation_summary.flagged_percent / 100)})</span>
                <span><strong>{report.data.reputation_summary.dnsbl_hit_ips}</strong> DNSBL hits</span>
              </div>
              <RiskHistogram data={report.data.risk_histogram} />
              <section className="report-section"><h2>IP reputation</h2><IPTable rows={report.data.ips} /></section>
            </>}
          </ReportBoundary>
        </TabsContent>
        <TabsContent className="tab-content" value="samples">
          <section className="report-section"><h2>Sample history</h2>
            {samples.isPending ? <SampleLoading /> : samples.isError ? <Alert><AlertTriangle aria-hidden="true" /><div><strong>Could not load samples</strong><p>{errorMessage(samples.error)}</p></div><Button type="button" onClick={() => samples.refetch()}>Retry samples</Button></Alert> : samples.data && <GroupedSamplesTable page={samples.data} />}
          </section>
        </TabsContent>
      </Tabs>
    </main>
  )
}

function ReportBoundary({ report, children }: { report: ReturnType<typeof useSessionReport>; children: React.ReactNode }) {
  if (report.isPending) return <ReportLoading />
  if (report.isError) return <Alert><AlertTriangle aria-hidden="true" /><div><strong>Could not load report</strong><p>{errorMessage(report.error)}</p></div><Button type="button" onClick={() => report.refetch()}>Retry report</Button></Alert>
  return children
}

function SessionLoading({ includeSamples }: { includeSamples: boolean }) {
  return <main className="session-page session-loading"><div role="status" aria-label="Loading session detail"><span className="sr-only">Loading session detail</span><Skeleton className="session-heading-skeleton" /><Skeleton className="session-subheading-skeleton" /></div><ReportLoading />{includeSamples && <SampleLoading />}</main>
}

function ReportLoading() {
  return <div className="report-loading" aria-label="Loading report"><Skeleton className="summary-skeleton" />{Array.from({ length: 4 }, (_, index) => <Skeleton key={index} className="chart-skeleton" />)}</div>
}

function SampleLoading() {
  return <div className="sample-loading" aria-label="Loading samples"><Skeleton className="sample-heading-skeleton" />{Array.from({ length: 4 }, (_, index) => <Skeleton key={index} className="sample-row-skeleton" />)}</div>
}

function NotFound() {
  return <main className="empty-state"><h1>Session not found</h1><p>This session may have been removed, or the link may be incomplete.</p><Link to="/" className="button button-primary">Back to sessions</Link></main>
}

function positiveInteger(value: string | null) {
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : 1
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : 'The request could not be completed.'
}

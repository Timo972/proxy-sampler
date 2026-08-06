import { AlertTriangle, Plus, RotateCw } from 'lucide-react'
import { Link, useNavigate } from 'react-router-dom'

import { type Run, useRuns, useSessions, useStopSession } from '../lib/api'
import { formatTimestamp } from '../lib/format'
import { SessionTable } from '../components/session-table'
import { SessionStatusBadge } from '../components/session-status-badge'
import { Alert } from '../components/ui/alert'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '../components/ui/table'

export function HomePage({ onNewSession, onNewRun }: { onNewSession: () => void; onNewRun: () => void }) {
  const sessions = useSessions()
  const stop = useStopSession()
  const runs = useRuns()

  if (sessions.isPending) return <SessionSkeleton />
  if (sessions.isError) {
    return (
      <main className="home-page">
        <Alert className="error-state">
          <AlertTriangle aria-hidden="true" size={18} />
          <div><strong>Sessions are unavailable</strong><p>{sessions.error.message}</p></div>
          <Button type="button" variant="secondary" onClick={() => sessions.refetch()}><RotateCw aria-hidden="true" size={15} /> Retry</Button>
        </Alert>
      </main>
    )
  }

  const active = sessions.data.filter((session) => session.status === 'running')
  const inactive = sessions.data.filter((session) => session.status !== 'running')
  const stoppingID = stop.isPending ? stop.variables : undefined

  return (
    <main className="home-page">
      {stop.isError && (
        <Alert className="inline-alert">
          <AlertTriangle aria-hidden="true" size={18} />
          <div><strong>Session could not be stopped</strong><p>{stop.error.message}</p></div>
        </Alert>
      )}
      <RunsSection runs={runs} onNewRun={onNewRun} />
      {sessions.data.length === 0 ? (
        <section className="empty-state" aria-labelledby="empty-heading">
          <h1 id="empty-heading">Start your first sampling session</h1>
          <p>Sticky sessions measure how long one egress IP is held. Pool sessions measure per-request rotation and how the available pool grows.</p>
          <Button type="button" variant="primary" onClick={onNewSession}><Plus aria-hidden="true" size={16} /> New session</Button>
        </section>
      ) : (
        <div className="session-sections">
          <h1 className="sr-only">Sampling sessions</h1>
          <SessionTable id="active-sessions" title="Active sessions" emptyMessage="No sessions are currently running." sessions={active} stoppingID={stoppingID} onStop={(session) => stop.mutate(session.id)} />
          <SessionTable id="inactive-sessions" title="Stopped / Finished sessions" emptyMessage="Stopped and finished sessions will appear here." sessions={inactive} stoppingID={stoppingID} onStop={(session) => stop.mutate(session.id)} />
        </div>
      )}
    </main>
  )
}

function RunsSection({ runs, onNewRun }: { runs: ReturnType<typeof useRuns>; onNewRun: () => void }) {
  const navigate = useNavigate()
  return (
    <section className="runs-section" aria-labelledby="runs-heading">
      <div className="section-heading">
        <h2 id="runs-heading">Parameter-variation runs</h2>
        <span className="section-count" aria-label={`${runs.data?.length ?? 0} runs`}>{runs.data?.length ?? 0}</span>
        <Button type="button" variant="secondary" size="small" className="section-action" onClick={onNewRun}><Plus aria-hidden="true" size={15} /> New run</Button>
      </div>
      {runs.isPending ? (
        <div className="skeleton-list">
          <div className="skeleton-row" aria-label="Loading runs"><Skeleton /><Skeleton /><Skeleton /><Skeleton /></div>
        </div>
      ) : runs.isError ? (
        <Alert className="inline-alert">
          <AlertTriangle aria-hidden="true" size={18} />
          <div><strong>Runs are unavailable</strong><p>{runs.error.message}</p></div>
          <Button type="button" variant="secondary" onClick={() => runs.refetch()}><RotateCw aria-hidden="true" size={15} /> Retry</Button>
        </Alert>
      ) : runs.data.length === 0 ? (
        <p className="section-empty">No parameter-variation runs yet. A run expands one template across several proxy configurations.</p>
      ) : (
        <div className="data-table-wrap">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Variants</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Distinct IPs</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.data.map((run: Run) => (
                <TableRow key={run.id} className="session-row" onClick={() => navigate(`/runs/${run.id}`)}>
                  <TableCell><Link className="session-link" to={`/runs/${run.id}`} onClick={(event) => event.stopPropagation()}><strong>{run.name}</strong></Link></TableCell>
                  <TableCell className="numeric">{run.variant_count}</TableCell>
                  <TableCell><SessionStatusBadge status={run.status} /></TableCell>
                  <TableCell className="numeric">{run.distinct_ips}</TableCell>
                  <TableCell>{formatTimestamp(run.created_at)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  )
}

function SessionSkeleton() {
  return (
    <main className="home-page" role="status" aria-label="Loading sessions">
      <span className="sr-only">Loading sessions</span>
      <div className="skeleton-heading"><Skeleton /><Skeleton /></div>
      <div className="skeleton-list">
        {Array.from({ length: 4 }, (_, index) => (
          <div className="skeleton-row" aria-label="Loading session" key={index}>
            <Skeleton /><Skeleton /><Skeleton /><Skeleton />
          </div>
        ))}
      </div>
    </main>
  )
}

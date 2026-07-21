import { AlertTriangle, Plus, RotateCw } from 'lucide-react'

import { useSessions, useStopSession } from '../lib/api'
import { SessionTable } from '../components/session-table'
import { Alert } from '../components/ui/alert'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'

export function HomePage({ onNewSession }: { onNewSession: () => void }) {
  const sessions = useSessions()
  const stop = useStopSession()

  if (sessions.isPending) return <SessionSkeleton />
  if (sessions.isError) {
    return (
      <main className="home-page no-page-overflow">
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
    <main className="home-page no-page-overflow">
      {stop.isError && (
        <Alert className="inline-alert">
          <AlertTriangle aria-hidden="true" size={18} />
          <div><strong>Session could not be stopped</strong><p>{stop.error.message}</p></div>
        </Alert>
      )}
      {sessions.data.length === 0 ? (
        <section className="empty-state" aria-labelledby="empty-heading">
          <h1 id="empty-heading">Start your first sampling session</h1>
          <p>Sticky sessions measure how long one egress IP is held. Pool sessions measure per-request rotation and how the available pool grows.</p>
          <Button type="button" variant="primary" onClick={onNewSession}><Plus aria-hidden="true" size={16} /> New session</Button>
        </section>
      ) : (
        <div className="session-sections">
          <SessionTable id="active-sessions" title="Active sessions" sessions={active} stoppingID={stoppingID} onStop={(session) => stop.mutate(session.id)} />
          <SessionTable id="inactive-sessions" title="Stopped / Finished sessions" sessions={inactive} stoppingID={stoppingID} onStop={(session) => stop.mutate(session.id)} />
        </div>
      )}
    </main>
  )
}

function SessionSkeleton() {
  return (
    <main className="home-page no-page-overflow" role="status" aria-label="Loading sessions">
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

import type { MouseEvent } from 'react'
import { Square } from 'lucide-react'
import { Link, useNavigate } from 'react-router-dom'

import type { Session } from '../lib/api'
import { formatLastIP, formatMilliseconds, formatPercent, formatTimestamp } from '../lib/format'
import { SessionStatusBadge } from './session-status-badge'
import { Button } from './ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from './ui/table'

interface SessionTableProps {
  id: string
  title: string
  sessions: Session[]
  stoppingID?: string
  onStop: (session: Session) => void
}

const columns = ['Name', 'Proxy', 'Mode', 'Status', 'Success rate', 'Median RTT', 'Distinct IPs', 'Last IP / category', 'Last sample']

export function SessionTable({ id, title, sessions, stoppingID, onStop }: SessionTableProps) {
  const navigate = useNavigate()
  const open = (session: Session) => navigate(`/sessions/${session.id}`)
  const stop = (event: MouseEvent, session: Session) => {
    event.stopPropagation()
    onStop(session)
  }

  return (
    <section className="session-section" aria-labelledby={id}>
      <div className="section-heading">
        <h2 id={id}>{title}</h2>
        <span className="section-count" aria-label={`${sessions.length} sessions`}>{sessions.length}</span>
      </div>
      <div className="session-table-wrap">
        <Table>
          <TableHeader>
            <TableRow>
              {columns.map((column) => <TableHead key={column}>{column}</TableHead>)}
              <TableHead><span className="sr-only">Actions</span></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sessions.map((session) => (
              <TableRow
                key={session.id}
                className="session-row"
                onClick={() => open(session)}
              >
                <TableCell><Link className="session-link" to={`/sessions/${session.id}`} onClick={(event) => event.stopPropagation()}><strong>{session.name}</strong></Link></TableCell>
                <TableCell className="mono">{session.proxy_display}</TableCell>
                <TableCell className="capitalize">{session.mode}</TableCell>
                <TableCell><SessionStatusBadge status={session.status} /></TableCell>
                <TableCell className="numeric">{formatPercent(session.success_rate)}</TableCell>
                <TableCell className="numeric">{formatMilliseconds(session.last_rtt_ms)}</TableCell>
                <TableCell className="numeric">{session.distinct_ips}</TableCell>
                <TableCell className="mono compact-cell">{formatLastIP(session.last_primary_ip, session.last_primary_category)}</TableCell>
                <TableCell>{formatTimestamp(session.last_sample_at)}</TableCell>
                <TableCell className="row-action">
                  {session.status === 'running' && (
                    <Button
                      type="button"
                      variant="quiet"
                      size="icon"
                      aria-label={`Stop ${session.name}`}
                      disabled={stoppingID === session.id}
                      onClick={(event) => stop(event, session)}
                    ><Square aria-hidden="true" size={15} /></Button>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <div className="session-records">
        {sessions.map((session) => (
          <article className="session-record" key={session.id}>
            <div className="record-heading">
              <Link className="record-title" to={`/sessions/${session.id}`}>{session.name}</Link>
              <SessionStatusBadge status={session.status} />
            </div>
            <dl>
              <Record label="Proxy" value={session.proxy_display} mono />
              <Record label="Mode" value={session.mode} />
              <Record label="Status" value={session.status} />
              <Record label="Success rate" value={formatPercent(session.success_rate)} />
              <Record label="Median RTT" value={formatMilliseconds(session.last_rtt_ms)} />
              <Record label="Distinct IPs" value={String(session.distinct_ips)} />
              <Record label="Last IP / category" value={formatLastIP(session.last_primary_ip, session.last_primary_category)} mono />
              <Record label="Last sample" value={formatTimestamp(session.last_sample_at)} />
            </dl>
            {session.status === 'running' && (
              <Button type="button" variant="secondary" disabled={stoppingID === session.id} onClick={(event) => stop(event, session)}>
                <Square aria-hidden="true" size={15} /> Stop session
              </Button>
            )}
          </article>
        ))}
      </div>
    </section>
  )
}

function Record({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return <div><dt>{label}</dt><dd className={mono ? 'mono' : undefined}>{value}</dd></div>
}

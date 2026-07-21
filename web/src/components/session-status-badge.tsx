import type { SessionStatus } from '../lib/api'
import { Badge } from './ui/badge'

export function SessionStatusBadge({ status }: { status: SessionStatus }) {
  return <Badge className={`status-badge status-${status}`}>{status === 'running' ? 'Running' : status === 'stopped' ? 'Stopped' : 'Finished'}</Badge>
}

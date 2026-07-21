import type { ReactNode } from 'react'
import { Activity, Plus } from 'lucide-react'

import { useReadiness } from '../lib/api'
import { Button } from './ui/button'

export function AppShell({ children, onNewSession }: { children: ReactNode; onNewSession: () => void }) {
  const readiness = useReadiness()
  const ready = readiness.isSuccess && readiness.data.postgres && readiness.data.clickhouse
  const readinessLabel = readiness.isPending ? 'Checking services' : ready ? 'Services ready' : 'Services unavailable'

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="topbar-inner">
          <a href="/" className="product-name" aria-label="Proxy Sampler home">
            <Activity aria-hidden="true" size={18} />
            <span>Proxy Sampler</span>
          </a>
          <div className="topbar-actions">
            <div className={`readiness ${ready ? 'is-ready' : readiness.isPending ? 'is-pending' : 'is-unavailable'}`} role="status" aria-label={readinessLabel} aria-live="polite">
              <span className="readiness-dot" aria-hidden="true" />
              <span>{readinessLabel}</span>
            </div>
            <Button type="button" variant="primary" onClick={onNewSession}><Plus aria-hidden="true" size={16} /> New session</Button>
          </div>
        </div>
      </header>
      <div className="app-content">{children}</div>
    </div>
  )
}

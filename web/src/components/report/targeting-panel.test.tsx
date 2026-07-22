import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { TargetingPanel } from './targeting-panel'
import type { Session, SessionReport } from '../../lib/api'

const session = { proxy_username: 'country-us-type-resi', proxy_display: 'p.example:8080' } as Session
const report = {
  series: [], stickiness: { holds: [], rotations: [], average_hold_seconds: 0, median_hold_seconds: 0 },
  pool_growth: [], pool_composition: { mobile: 0, residential: 1, datacenter: 0, unknown: 0 },
  reputation_summary: { total_ips: 0, flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0 },
  risk_histogram: [], ips: [{ ip: '1.1.1.1', category: 'residential', country: 'US', isp: '', asn: '', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 5 }],
} as SessionReport

describe('TargetingPanel', () => {
  it('renders parsed attribute chips and a comparison', () => {
    render(<TargetingPanel session={session} report={report} />)
    // 'Country' and 'US' each legitimately appear twice (chip label + comparison
    // row, and requested + observed columns), so assert presence rather than a
    // single unique match.
    expect(screen.getAllByText('Country').length).toBeGreaterThan(0)
    expect(screen.getAllByText(/US/).length).toBeGreaterThan(0)
  })

  it('renders a fallback when the proxy has no username', () => {
    render(<TargetingPanel session={{ ...session, proxy_username: null }} report={report} />)
    expect(screen.getByText(/no username/i)).toBeInTheDocument()
  })
})

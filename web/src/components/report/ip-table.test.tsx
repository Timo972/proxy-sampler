import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import type { IPRow } from '../../lib/api'
import { IPTable } from './ip-table'

const rows = [
  {
    ip: '203.0.113.7',
    category: 'residential',
    country: 'DE',
    isp: 'Example Broadband With A Full Unclipped Name',
    asn: 'AS64500 Example Broadband Network',
    risk_score: 12,
    greynoise_class: 'benign',
    dnsbl_listed: false,
    dnsbl_hits: [],
    first_seen: '2026-07-21T12:00:00Z',
    last_seen: '2026-07-21T12:03:00Z',
    hit_count: 4,
  },
  {
    ip: '198.51.100.9',
    category: 'datacenter',
    country: 'DE',
    isp: 'Example Transit With A Full Unclipped Name',
    asn: 'AS64501 Example Transit Network',
    risk_score: 82,
    greynoise_class: 'malicious',
    dnsbl_listed: true,
    dnsbl_hits: ['zen.spamhaus.org'],
    first_seen: '2026-07-21T12:01:00Z',
    last_seen: '2026-07-21T12:04:00Z',
    hit_count: 8,
  },
] satisfies IPRow[]

describe('IPTable', () => {
  it('sorts by hits and exposes the complete ISP through a portalled tooltip', async () => {
    render(<IPTable rows={rows} />)

    const tableRows = within(screen.getByRole('table')).getAllByRole('row')
    expect(tableRows[1]).toHaveTextContent('198.51.100.9')
    expect(tableRows[2]).toHaveTextContent('203.0.113.7')
    expect(within(tableRows[1]).getByText('AS64501 Example Transit Network')).toBeInTheDocument()

    const isp = within(tableRows[1]).getByRole('button', { name: 'Full ISP for 198.51.100.9' })
    await userEvent.hover(isp)
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Example Transit With A Full Unclipped Name')
  })

  it('exposes the complete OpenAPI ASN string through a portalled tooltip', async () => {
    render(<IPTable rows={rows} />)

    const asn = screen.getByRole('button', { name: 'Full ASN for 198.51.100.9' })
    await userEvent.hover(asn)
    expect(await screen.findByRole('tooltip')).toHaveTextContent('AS64501 Example Transit Network')
  })

  it('keeps IP, category, and risk visible while mobile details expand progressively', async () => {
    render(<IPTable rows={rows} />)

    const trigger = screen.getByRole('button', { name: 'Show details for 198.51.100.9' })
    const record = trigger.closest('article')
    expect(record).not.toBeNull()
    expect(within(record!).getByText('198.51.100.9')).toBeInTheDocument()
    expect(within(record!).getByText('datacenter')).toBeInTheDocument()
    expect(within(record!).getByText('High 82')).toBeInTheDocument()
    expect(trigger).toHaveAttribute('aria-expanded', 'false')

    await userEvent.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    expect(within(record!).getByText('Example Transit With A Full Unclipped Name')).toBeInTheDocument()
    expect(within(record!).getByText('AS64501 Example Transit Network')).toBeInTheDocument()
    expect(within(record!).getByText('Hits')).toBeInTheDocument()
    expect(within(record!).getByText('8')).toBeInTheDocument()
  })
})

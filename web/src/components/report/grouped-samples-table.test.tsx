import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'

import { GroupedSamplesTable } from './grouped-samples-table'

const newest = {
  sample_seq: 12,
  sampled_at: '2026-07-21T12:04:00Z',
  probes_attempted: 3,
  probes_ok: 2,
  primary_ip: '203.0.113.7',
  distinct_ips: 2,
  ip_changed: true,
  new_ips: 1,
  rtt_min_ms: 101,
  rtt_med_ms: 142,
  rtt_max_ms: 205,
  egress_country: 'DE',
  primary_category: 'residential',
  primary_risk: 12,
  probe_ips: ['203.0.113.7', null, '198.51.100.9'],
  probe_rtts_ms: [101, 0, 205],
  probe_ok: [true, false, true],
  error: 'probe 1 timed out',
}

const older = {
  ...newest,
  sample_seq: 11,
  sampled_at: '2026-07-21T12:03:00Z',
  ip_changed: false,
  new_ips: 0,
}

describe('GroupedSamplesTable', () => {
  it('sorts parent rows newest-first and discloses exactly the attempted probes in source order', async () => {
    renderTable({ items: [older, newest], page: 1, page_size: 50, total: 2 })

    const disclosures = screen.getAllByRole('button', { name: /sample \d+ details/i })
    expect(disclosures.map((button) => button.textContent)).toEqual(['Sample 12', 'Sample 11'])
    expect(disclosures[0]).toHaveAttribute('aria-expanded', 'false')
  const parent = disclosures[0].closest('tr')
  expect(parent).not.toBeNull()
  expect(within(parent!).getByText('Changed')).toHaveClass('badge')
  expect(within(parent!).getByText('probe 1 timed out')).toBeInTheDocument()

    await userEvent.click(disclosures[0])
    expect(disclosures[0]).toHaveAttribute('aria-expanded', 'true')
    const probes = screen.getAllByRole('row', { name: /Probe \d+ for sample 12/i })
    expect(probes).toHaveLength(3)
    expect(within(probes[0]).getByText('203.0.113.7')).toBeInTheDocument()
    expect(within(probes[0]).getByText('101 ms')).toBeInTheDocument()
    expect(within(probes[1]).getByText('Failed')).toBeInTheDocument()
    expect(within(probes[1]).getByText('probe 1 timed out')).toBeInTheDocument()
    expect(within(probes[2]).getByText('198.51.100.9')).toBeInTheDocument()
    expect(within(probes[2]).getByText('205 ms')).toBeInTheDocument()
  })

  it('treats probes_attempted as authoritative when parallel arrays are mismatched', async () => {
    const mismatched = {
      ...newest,
      probes_attempted: 4,
      probe_ips: ['203.0.113.7'],
      probe_rtts_ms: [101, 202],
      probe_ok: [true, false, true],
      error: 'incomplete probe vectors',
    }
    renderTable({ items: [mismatched], page: 1, page_size: 50, total: 1 })

    await userEvent.click(screen.getByRole('button', { name: 'Sample 12 details' }))
    const probes = screen.getAllByRole('row', { name: /Probe \d+ for sample 12/i })
    expect(probes).toHaveLength(4)
    expect(within(probes[2]).getAllByText('—')).toHaveLength(3)
    expect(within(probes[3]).getAllByText('—')).toHaveLength(2)
    expect(within(probes[3]).getByText('Failed')).toBeInTheDocument()
  })

  it('restores focus to the disclosure button after collapse', async () => {
    renderTable({ items: [newest], page: 1, page_size: 50, total: 1 })
    const disclosure = screen.getByRole('button', { name: 'Sample 12 details' })
    await userEvent.click(disclosure)
    await userEvent.click(disclosure)

    expect(disclosure).toHaveAttribute('aria-expanded', 'false')
    expect(disclosure).toHaveFocus()
    expect(screen.queryByRole('row', { name: 'Probe 0 for sample 12' })).not.toBeInTheDocument()
  })

  it('preserves the samples tab when changing the current fixed page', async () => {
    renderTable({ items: [newest], page: 2, page_size: 50, total: 140 }, '?tab=samples&page=2')

    expect(screen.getByText('Page 2 of 3')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Next page' }))
    expect(screen.getByTestId('sample-location')).toHaveTextContent('?tab=samples&page=3')
    await userEvent.click(screen.getByRole('button', { name: 'Previous page' }))
    expect(screen.getByTestId('sample-location')).toHaveTextContent('?tab=samples&page=2')
  })

  it('renders a useful empty state instead of an empty table', () => {
    renderTable({ items: [], page: 1, page_size: 50, total: 0 })
    expect(screen.getByText('Samples will appear after the first sampling interval completes.')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })
})

function renderTable(page: Parameters<typeof GroupedSamplesTable>[0]['page'], search = '?tab=samples&page=1') {
  return render(
    <MemoryRouter initialEntries={[`/sessions/session-id${search}`]}>
      <GroupedSamplesTable page={page} />
      <LocationOutput />
    </MemoryRouter>,
  )
}

function LocationOutput() {
  const location = useLocation()
  return <output data-testid="sample-location">{location.search}</output>
}

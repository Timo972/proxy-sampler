import { describe, expect, it } from 'vitest'

import { compareAttributes, parseProxyAttributes } from './proxy-attributes'
import type { SessionReport } from './api'

describe('parseProxyAttributes', () => {
  it('recognizes mixed-delimiter key/value attributes and groups them', () => {
    const parsed = parseProxyAttributes('user-country-us_city-newyork:type-resi-session-ab12')
    const byKey = Object.fromEntries(parsed.attributes.map((a) => [a.key, a]))
    expect(byKey.country.value).toBe('us')
    expect(byKey.country.category).toBe('geo')
    expect(byKey.city.value).toBe('newyork')
    expect(byKey.type.category).toBe('network')
    expect(byKey.session.category).toBe('session')
    // 'user' is an unrecognized leading token.
    expect(parsed.raw).toContain('user')
  })

  it('returns empty structures for empty input and never throws', () => {
    expect(parseProxyAttributes(null)).toEqual({ attributes: [], raw: [] })
    expect(parseProxyAttributes('')).toEqual({ attributes: [], raw: [] })
    expect(() => parseProxyAttributes('---___:::')).not.toThrow()
  })

  it('treats Object.prototype-named tokens as unrecognized rather than mis-parsing them', () => {
    const parsed = parseProxyAttributes('country-us-constructor-x')
    const byKey = Object.fromEntries(parsed.attributes.map((a) => [a.key, a]))
    expect(byKey.country.value).toBe('us')
    expect(parsed.raw).toContain('constructor')
    expect(parsed.raw).toContain('x')
    for (const attr of parsed.attributes) {
      expect(attr.category).not.toBeUndefined()
      expect(attr.label).not.toBeUndefined()
    }
  })
})

describe('compareAttributes', () => {
  const report: SessionReport = {
    series: [],
    stickiness: { holds: [], rotations: [], average_hold_seconds: 0, median_hold_seconds: 0 },
    pool_growth: [],
    pool_composition: { mobile: 0.1, residential: 0.8, datacenter: 0.1, unknown: 0 },
    reputation_summary: { total_ips: 0, flagged_ips: 0, flagged_percent: 0, dnsbl_hit_ips: 0 },
    risk_histogram: [],
    ips: [
      { ip: '203.0.113.1', category: 'residential', country: 'US', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 9 },
      { ip: '203.0.113.2', category: 'residential', country: 'DE', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 1 },
    ],
  }

  it('marks a satisfied country request as a match', () => {
    const rows = compareAttributes(parseProxyAttributes('country-us'), report)
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.requested).toBe('US')
    expect(country.observed).toContain('US')
    expect(country.verdict).toBe('match')
  })

  it('marks a network-type mismatch', () => {
    const rows = compareAttributes(parseProxyAttributes('type-mobile'), report)
    const net = rows.find((r) => r.label === 'Network type')!
    expect(net.verdict).toBe('mismatch') // observed is dominantly residential
  })

  it('shows requested-only rows for attributes with no observed counterpart', () => {
    const rows = compareAttributes(parseProxyAttributes('city-newyork'), report)
    const city = rows.find((r) => r.label === 'City')!
    expect(city.observed).toBe('—')
    expect(city.verdict).toBe('unknown')
  })

  it('marks a country request as partial when the dominant share is below the match threshold', () => {
    const mixedReport: SessionReport = {
      ...report,
      ips: [
        { ip: '203.0.113.1', category: 'residential', country: 'US', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 6 },
        { ip: '203.0.113.2', category: 'residential', country: 'DE', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 4 },
      ],
    }
    const rows = compareAttributes(parseProxyAttributes('country-us'), mixedReport)
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.observed).toContain('US')
    expect(country.verdict).toBe('partial')
  })
})

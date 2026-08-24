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

  it('uses the most-common observed ISP rather than the first row', () => {
    const ispReport: SessionReport = {
      ...report,
      ips: [
        // First row is the less-frequent ISP, so a naive ips[0] lookup would report "Other".
        { ip: '203.0.113.1', category: 'residential', country: 'DE', isp: 'Other', asn: 'AS2', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 3 },
        { ip: '203.0.113.2', category: 'residential', country: 'US', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 9 },
        { ip: '203.0.113.3', category: 'residential', country: 'US', isp: 'Acme', asn: 'AS1', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 8 },
      ],
    }
    const rows = compareAttributes(parseProxyAttributes('isp-acme'), ispReport)
    const isp = rows.find((r) => r.label === 'ISP')!
    // Acme's combined tally (9 + 8 = 17) beats Other's (3), even though Other appears first.
    expect(isp.observed).toContain('Acme')
    expect(isp.verdict).toBe('match')
  })

  it('compares against a manual target country when the username has none', () => {
    const rows = compareAttributes(parseProxyAttributes('session-ab12'), report, 'US')
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.requested).toContain('US')
    expect(country.requested).toContain('manual')
    expect(country.observed).toContain('US')
    expect(country.verdict).toBe('match')
  })

  it('lets a manual target country override the username country', () => {
    const rows = compareAttributes(parseProxyAttributes('country-us'), report, 'DE')
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.requested).toContain('DE')
    // Dominant observed is US, so the overriding manual target de mismatches.
    expect(country.verdict).toBe('mismatch')
  })

  it('notes the username country that a conflicting manual target overrides', () => {
    const rows = compareAttributes(parseProxyAttributes('country-us'), report, 'DE')
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.requested).toContain('DE (manual, overrides US)')
  })

  it('does not claim an override when the manual target equals the username country', () => {
    const rows = compareAttributes(parseProxyAttributes('country-de'), report, 'DE')
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.requested).toBe('DE (manual)')
  })

  it('buckets observed country codes case-insensitively for the dominant share', () => {
    const mixedCase: SessionReport = {
      ...report,
      ips: [
        { ip: '203.0.113.1', category: 'residential', country: 'DE', isp: '', asn: '', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 6 },
        { ip: '203.0.113.2', category: 'residential', country: 'de', isp: '', asn: '', risk_score: null, greynoise_class: '', dnsbl_listed: false, dnsbl_hits: [], first_seen: '', last_seen: '', hit_count: 5 },
      ],
    }
    const rows = compareAttributes(parseProxyAttributes(''), mixedCase, 'DE')
    const country = rows.find((r) => r.label === 'Country')!
    // All traffic is DE; a case-split vote must not downgrade the verdict.
    expect(country.observed).toContain('100%')
    expect(country.verdict).toBe('match')
  })

  it('reports an unknown verdict for a manual target with no observations', () => {
    const rows = compareAttributes(parseProxyAttributes(''), { ...report, ips: [] }, 'DE')
    const country = rows.find((r) => r.label === 'Country')!
    expect(country.observed).toBe('—')
    expect(country.verdict).toBe('unknown')
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

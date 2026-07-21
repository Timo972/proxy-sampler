import { describe, expect, it } from 'vitest'

import { formatLastIP, formatMilliseconds, formatTimestamp } from './format'

describe('format helpers', () => {
  it('renders omitted sample values as unavailable', () => {
    expect(formatMilliseconds(undefined)).toBe('—')
    expect(formatLastIP(undefined, undefined)).toBe('—')
    expect(formatTimestamp(undefined)).toBe('—')
  })

  it('does not throw for an invalid timestamp', () => {
    expect(formatTimestamp('not-a-date')).toBe('—')
  })
})

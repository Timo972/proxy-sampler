import { describe, expect, it } from 'vitest'
import { variantCount } from './variation'
import type { AxisSpec } from './api'

describe('variantCount', () => {
  it('multiplies list, range, and random axes', () => {
    const axes: Record<string, AxisSpec> = {
      country: { kind: 'list', values: ['de', 'us', 'fr'] },
      port: { kind: 'range', from: 10000, to: 10004 },
      session: { kind: 'random', count: 2 },
    }
    expect(variantCount(axes)).toBe(3 * 5 * 2)
  })

  it('treats an empty list axis as zero', () => {
    expect(variantCount({ c: { kind: 'list', values: [] } })).toBe(0)
  })

  it('returns 1 for no axes', () => {
    expect(variantCount({})).toBe(1)
  })

  it('counts an inclusive range', () => {
    expect(variantCount({ p: { kind: 'range', from: 1, to: 1 } })).toBe(1)
  })
})

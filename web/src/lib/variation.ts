import type { AxisSpec } from './api'

// variantCount mirrors the backend expansion count without generating values.
export function variantCount(axes: Record<string, AxisSpec>): number {
  let total = 1
  for (const spec of Object.values(axes)) {
    total *= axisSize(spec)
  }
  return total
}

function axisSize(spec: AxisSpec): number {
  switch (spec.kind) {
    case 'list':
      return spec.values?.length ?? 0
    case 'range':
      if (spec.from === undefined || spec.to === undefined || spec.to < spec.from) return 0
      return spec.to - spec.from + 1
    case 'random':
      return spec.count && spec.count > 0 ? spec.count : 0
    default:
      return 0
  }
}

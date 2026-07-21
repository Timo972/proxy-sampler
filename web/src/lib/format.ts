export function formatPercent(value: number): string {
  return new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 1 }).format(value)
}

export function formatMilliseconds(value: number | null | undefined): string {
  return value == null || !Number.isFinite(value) ? '—' : `${new Intl.NumberFormat().format(value)} ms`
}

export function formatTimestamp(value: string | null | undefined): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(date)
}

export function formatLastIP(ip: string | null | undefined, category: string | null | undefined): string {
  if (!ip) return '—'
  return category ? `${ip} · ${category}` : ip
}

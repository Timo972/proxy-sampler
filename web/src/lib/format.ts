export function formatPercent(value: number): string {
  return new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 1 }).format(value)
}

export function formatMilliseconds(value: number | null): string {
  return value === null ? '—' : `${new Intl.NumberFormat().format(value)} ms`
}

export function formatTimestamp(value: string | null): string {
  if (value === null) return '—'
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value))
}

export function formatLastIP(ip: string | null, category: string | null): string {
  if (ip === null) return '—'
  return category ? `${ip} · ${category}` : ip
}

const UNITS = ["B", "KB", "MB", "GB"]

export function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), UNITS.length - 1)
  const value = bytes / 1024 ** exponent
  return `${value >= 10 ? Math.round(value) : value.toFixed(1)} ${UNITS[exponent]}`
}

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" })
const DIVISIONS: [number, Intl.RelativeTimeFormatUnit][] = [
  [60, "second"],
  [60, "minute"],
  [24, "hour"],
  [7, "day"],
  [4.345, "week"],
  [12, "month"],
  [Infinity, "year"],
]

/** "3 hours ago", "yesterday", "last month". Uses the browser's locale. */
export function formatRelativeTime(iso: string): string {
  let duration = (new Date(iso).getTime() - Date.now()) / 1000
  for (const [amount, unit] of DIVISIONS) {
    if (Math.abs(duration) < amount) return relative.format(Math.round(duration), unit)
    duration /= amount
  }
  return ""
}

export function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })
}
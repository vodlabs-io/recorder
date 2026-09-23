import type { MoveFailureReason, VOD } from "@/lib/daemon"

// zero is what a missing or zero size renders as. It defaults to an em dash
// because a card's size is unknown rather than empty, but a running total
// counting up from nothing wants a literal "0 B".
export function formatBytes(n: number, zero = "—"): string {
  if (!n || n <= 0) return zero
  const units = ["B", "KB", "MB", "GB", "TB"]
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

export function formatDurationSec(sec?: number): string {
  if (!sec || sec <= 0) return "—"
  const s = Math.round(sec)
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (h > 0) return `${h}h ${m.toString().padStart(2, "0")}m`
  if (m > 0) return `${m}m ${(s % 60).toString().padStart(2, "0")}s`
  return `${s}s`
}

export function formatDate(iso?: string): string {
  if (!iso) return "unknown date"
  const d = new Date(iso)
  if (isNaN(d.getTime())) return "unknown date"
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  })
}

// formatRecordedTime is a compact, age-adaptive alternative to formatDate for
// card display: recent sessions read at a glance ("Today", "Yesterday", a
// weekday), older ones fall back to a calendar date, and the year only
// appears once it stops being implied. Calendar-day boundaries (not a
// rolling 24h window) so labels stay stable as time passes rather than
// flipping mid-scroll. Returns the date and time parts separately so the
// caller can allow a line break between them (but never inside the time)
// when card width is tight.
export function formatRecordedTime(iso?: string): { date: string; time: string } {
  if (!iso) return { date: "unknown date", time: "" }
  const d = new Date(iso)
  if (isNaN(d.getTime())) return { date: "unknown date", time: "" }

  const now = new Date()
  const startOfDay = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const dayDiff = Math.round((startOfDay(now) - startOfDay(d)) / (24 * 3600 * 1000))

  const time = d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" })
  let date: string
  if (dayDiff <= 0) {
    date = "Today"
  } else if (dayDiff === 1) {
    date = "Yesterday"
  } else if (dayDiff < 7) {
    date = d.toLocaleDateString(undefined, { weekday: "short" })
  } else if (d.getFullYear() === now.getFullYear()) {
    date = d.toLocaleDateString(undefined, { month: "short", day: "numeric" })
  } else {
    date = d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
    })
  }
  return { date, time }
}

// sessionTime is the timestamp a session is ordered, filtered, and labeled by:
// its start, falling back to its end, since a manifest may carry only `end`.
export function sessionTime(s: VOD): string | undefined {
  return s.start ?? s.end
}

// sessionKey uniquely identifies a session row (session_id can be empty for
// folders without a manifest, but dir is always unique).
export function sessionKey(s: VOD): string {
  return s.dir
}

// parseLocalDate turns a date-input value ("2026-07-15") into local midnight.
// new Date("2026-07-15") would parse as UTC midnight, shifting the boundary by
// the timezone offset — a calendar day the user picks must mean THEIR day.
export function parseLocalDate(value: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  if (!m) return null
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]))
  return isNaN(d.getTime()) ? null : d
}

// formatDateValue is parseLocalDate's inverse: a local Date to "YYYY-MM-DD".
export function formatDateValue(d: Date): string {
  const p = (n: number) => n.toString().padStart(2, "0")
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

export async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    // Clipboard API can be unavailable in non-secure contexts; fall back to
    // the legacy path so "Copy path" still works everywhere.
    const ta = document.createElement("textarea")
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand("copy")
    ta.remove()
  }
}

// formatBitrate reports a session's average bitrate across its whole duration.
// Averaged, not instantaneous: real bitrate swings scene to scene, and what
// this answers is the coarse question of whether a recording came down at the
// quality the variant promised.
export function formatBitrate(bytes: number, sec?: number): string {
  if (!bytes || !sec || sec <= 0) return "—"
  const bps = (bytes * 8) / sec
  if (bps >= 1e6) return `${(bps / 1e6).toFixed(1)} Mbps`
  if (bps >= 1e3) return `${Math.round(bps / 1e3)} kbps`
  return `${Math.round(bps)} bps`
}

// formatTimeOfDay is the clock part of a timestamp, for rows already grouped
// under a date — a session's parts span hours, not days, so repeating the
// date on every one of them is noise.
export function formatTimeOfDay(iso?: string): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (isNaN(d.getTime())) return "—"
  return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" })
}

// spanSec is a session's wall-clock length: first frame to last, INCLUDING any
// stretch where the stream dropped and the recorder was reconnecting.
// duration_sec is a different quantity — the media length ffprobe measured,
// which excludes those gaps (see internal/processing/manifest.go, which
// deliberately prefers the probed value over the wall-clock estimate). Both
// ends must be known; undefined otherwise.
export function spanSec(s: VOD): number | undefined {
  if (!s.start || !s.end) return undefined
  const a = new Date(s.start).getTime()
  const b = new Date(s.end).getTime()
  if (isNaN(a) || isNaN(b) || b <= a) return undefined
  return (b - a) / 1000
}

// Below this, the difference between wall-clock span and media duration is
// container overhead and rounding rather than a dropout worth reporting.
const GAP_FLOOR_SEC = 30

// gapSec is how much of a session's wall-clock span holds no video — the
// reconnect time between a stream dropping and the recorder picking it up
// again. Undefined when it can't be known or is too small to mean anything.
//
// It under-reports rather than over-reports: builds without ffprobe fall back
// to storing the wall-clock estimate as duration_sec, which makes the gap
// vanish to zero. So a reported gap is real; an absent one is not proof of an
// unbroken recording.
export function gapSec(s: VOD): number | undefined {
  const span = spanSec(s)
  if (span === undefined || !s.duration_sec || s.duration_sec <= 0) return undefined
  const gap = span - s.duration_sec
  return gap >= GAP_FLOOR_SEC ? gap : undefined
}

// A move that is merely in progress says so; one that is stuck says why. The
// condition itself is reported once in the archive's banner — this is only the
// per-row hint that tells you which VODs are waiting on it.
export function moveReasonLabel(reason?: MoveFailureReason): string {
  switch (reason) {
    case "no_space":
      return "Moving (waiting for space)"
    case "unreachable":
      return "Moving (drive unavailable)"
    case "permission":
      return "Moving (permission denied)"
    default:
      return "Moving…"
  }
}

export function moveReasonDetail(reason?: MoveFailureReason): string | undefined {
  switch (reason) {
    case "no_space":
      return "The archive folder is full. This VOD moves as soon as space is freed."
    case "unreachable":
      return "The archive folder cannot be reached. This VOD moves when it is back."
    case "permission":
      return "The archive folder refused the write. Check its permissions."
    default:
      return undefined
  }
}


import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function getShortRelativeTime(isoDate: string): string {
  const date = new Date(isoDate);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffSec = Math.floor(diffMs / 1000);
  const diffMin = Math.floor(diffSec / 60);
  const diffHours = Math.floor(diffMin / 60);
  const diffDays = Math.floor(diffHours / 24);
  const diffWeeks = Math.floor(diffDays / 7);
  const diffMonths = Math.floor(diffDays / 30);
  const diffYears = Math.floor(diffDays / 365);

  if (diffYears > 0) return `${diffYears}y`;
  if (diffMonths > 0) return `${diffMonths}mo`;
  if (diffWeeks > 0) return `${diffWeeks}w`;
  if (diffDays > 0) return `${diffDays}d`;
  if (diffHours > 0) return `${diffHours}h`;
  if (diffMin > 0) return `${diffMin}m`;
  return `${diffSec}s`;
}

export function isValidDate(dateString: string): boolean {
  const dateObject = new Date(dateString)
  return !isNaN(dateObject.getTime())
}

// relativeTimeFromUnix renders compact relative time from unix seconds. Returns
// "" for null/0/NaN so empty timestamps render blank.
export function relativeTimeFromUnix(sec: number | null | undefined): string {
  if (!sec || Number.isNaN(sec)) return ""
  return getShortRelativeTime(new Date(sec * 1000).toISOString())
}

// formatBytes renders a byte count in human-readable units.
export function formatBytes(bytes: number): string {
  if (!bytes || bytes < 0) return "0 B"
  const units = ["B", "KB", "MB", "GB", "TB"]
  let value = bytes
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return `${value.toFixed(value >= 10 || i === 0 ? 0 : 1)} ${units[i]}`
}

// formatSpeed renders a bytes-per-second rate.
export function formatSpeed(bytesPerSec: number): string {
  return `${formatBytes(bytesPerSec)}/s`
}

// joinPath appends a file name to a session dir using the separator style the
// daemon's paths already use (the daemon may run on Windows).
export function joinPath(dir: string, name: string): string {
  const sep = dir.includes("\\") ? "\\" : "/"
  return dir.endsWith(sep) ? dir + name : dir + sep + name
}

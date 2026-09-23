import type { Activity, StreamerView } from "@/lib/daemon"
import type { IndicatorState } from "@/components/ui/status-indicator"

export type StatusKey =
  | "blocked_no_space"
  | "recording"
  | "lagging"
  | "live_paused"
  | "processing"
  | "moving"
  | "restricted"
  | "live"
  | "disabled"
  | "monitoring"
  | "idle"
  | "unavailable"
  | "defunct"
  | "invalid"

// statusKey derives the single status a streamer displays, applying the same
// precedence the Status column shows: disabled, then live-status, then activity.
// "paused" is reserved for the streamer pausing their own live, which displays
// as Live Paused — distinct from the user disabling recording for them.
export function statusKey(s: StreamerView): StatusKey {
  // Monitor on, record off: seeing who is live is the whole point of that
  // pairing, so being live outranks the Disabled label.
  if (!s.record) {
    return s.monitor && s.liveStatus === "live" ? "live" : "disabled"
  }
  if (s.liveStatus === "invalid") return "invalid"
  if (s.liveStatus === "unavailable") return "unavailable"
  if (s.liveStatus === "defunct") return "defunct"
  if (s.liveStatus === "restricted" && !sessionActive(s)) return "restricted"
  switch (s.activity) {
    case "blocked_no_space":
      return "blocked_no_space"
    case "recording":
      return "recording"
    case "paused":
      return "live_paused"
    case "lagging":
      return "lagging"
    case "processing":
      return "processing"
    case "moving":
    case "moving_retrying":
      return "moving"
    case "monitoring":
      // Live lands before the session flips activity to recording; report it
      // rather than hold "Monitoring" through the gap.
      return s.liveStatus === "live" ? "live" : "monitoring"
    default:
      return "idle"
  }
}

// sessionActive reports whether a recording is in progress.
function sessionActive(s: StreamerView): boolean {
  return (
    s.activity === "recording" ||
    s.activity === "lagging" ||
    s.activity === "paused"
  )
}

// oneShotEligible reports whether a monitored streamer may be switched to
// record-once right now. It mirrors the daemon's rule, which is the one that
// counts (vlapi's monitorOffAllowed): a one-shot exists to capture the run
// happening now, so it needs recording on and the streamer live. Switching it
// on for anyone else deletes the streamer — the switch moves it out of the
// config into an entry that expires unless a recording starts.
export function oneShotEligible(s: StreamerView): boolean {
  if (!s.record) return false
  // An active session outranks the live status, which lags a stream that just
  // came back.
  if (s.activity === "recording" || s.activity === "lagging") return true
  return s.liveStatus === "live" || s.liveStatus === "paused"
}

// oneShotBlockedReason explains a locked Monitor switch, in the terms of what
// the user has to do to unlock it.
export function oneShotBlockedReason(s: StreamerView): string {
  return s.record
    ? "Only while they're live"
    : "Enable recording first"
}

export const STATUS_LABELS: Record<StatusKey, string> = {
  blocked_no_space: "No Disk Space",
  recording: "Recording",
  lagging: "Lagging",
  live_paused: "Live Paused",
  processing: "Processing",
  moving: "Moving",
  restricted: "Restricted",
  live: "Live",
  disabled: "Disabled",
  monitoring: "Monitoring",
  idle: "Idle",
  unavailable: "Unavailable",
  defunct: "Defunct",
  invalid: "Invalid",
}

// STATUS_SORT_ORDER lists status keys from highest to lowest sort priority.
export const STATUS_SORT_ORDER: StatusKey[] = [
  // Sorted above everything: it is the only status that means footage is being
  // lost while you look at it.
  "blocked_no_space",
  "recording",
  "lagging",
  "live_paused",
  "processing",
  "moving",
  "live",
  "restricted",
  "disabled",
  "monitoring",
  "idle",
  "unavailable",
  "defunct",
  "invalid",
]

// isMoving reports whether a streamer has a finished recording on its way to the
// archive folder, retrying or not. Gates the Cancel move action.
export function isMoving(s: StreamerView): boolean {
  return s.activity === "moving" || s.activity === "moving_retrying"
}

// activityIndicator maps an activity to its tier and tooltip. Split out so the
// recording cards, which show only activity, tint their badges from the same
// mapping the row indicator uses.
export function activityIndicator(activity: Activity): {
  state: IndicatorState
  tooltip: string
} {
  switch (activity) {
    case "blocked_no_space":
      return {
        state: "error",
        tooltip: "Not recording: the download folder is out of space",
      }
    case "recording":
      return { state: "ok", tooltip: "Recording" }
    case "monitoring":
      return { state: "info", tooltip: "Monitoring" }
    case "processing":
      return { state: "info", tooltip: "Processing" }
    case "moving":
      return { state: "info", tooltip: "Moving to destination" }
    case "lagging":
      return { state: "degraded", tooltip: "Lagging" }
    case "paused":
      return { state: "degraded", tooltip: "Live Paused" }
    case "moving_retrying":
      return { state: "degraded", tooltip: "Moving to destination (retrying)" }
    case "idle":
    default:
      return { state: "idle", tooltip: "Idle" }
  }
}

// streamerIndicator maps a streamer to its status-indicator tier and an
// optional tooltip. Color carries severity, not activity type: green = works,
// blue = busy-and-normal, yellow = degraded, red glyph = fix your config,
// grey = intentionally inactive. "removed" gets no mapping — it's a transient
// teardown signal and the registry purges the streamer's KV moments later, so
// a row can only ever flash it (falls through to idle).
export function streamerIndicator(s: StreamerView): {
  state: IndicatorState
  pulse?: boolean
  tooltip?: string
} {
  if (!s.record) {
    // Live still reads as blue here — the label says Live, so a grey dot
    // beside it would contradict the row.
    if (s.monitor && s.liveStatus === "live") {
      return { state: "info", tooltip: "Live (recording disabled)" }
    }
    return { state: "idle", tooltip: "Recording Disabled" }
  }
  if (s.liveStatus === "invalid") {
    return { state: "error", tooltip: "Invalid" }
  }
  if (s.liveStatus === "unavailable") {
    return { state: "degraded", pulse: false, tooltip: "Account unavailable" }
  }
  if (s.liveStatus === "defunct") {
    return { state: "idle", tooltip: "Account gone; no longer checked" }
  }
  if (sessionActive(s)) return activityIndicator(s.activity)
  // Persistent degraded states: solid (no pulse) since nothing is actively
  // happening — they hold until a login/re-check clears them.
  if (s.liveStatus === "restricted") {
    return { state: "degraded", pulse: false, tooltip: "Restricted" }
  }
  if (s.liveStatus === "auth_required") {
    return { state: "degraded", pulse: false, tooltip: "Sign-in required" }
  }
  if (s.activity === "monitoring" && s.liveStatus === "live") {
    return { state: "info", tooltip: "Live" }
  }
  return activityIndicator(s.activity)
}

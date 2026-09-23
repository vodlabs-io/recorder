import {
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react"
import { fetchRecordings } from "@shared/daemon/recordings"
import { getDaemonClient, type VOD } from "@/lib/daemon"

export type VodsState = {
  sessions: VOD[]
  unavailableRoots: string[]
  // A daemon-side rescan is in flight; the listing may be stale/incomplete.
  daemonScanning: boolean
  // A client fetch is in flight (initial load, nudge, focus, manual refresh).
  loading: boolean
  // Download progress while paging a large listing, null when settled. The
  // sessions already in state remain renderable throughout.
  progress: { loaded: number; total: number } | null
  // At least one fetch has completed since connecting (distinguishes "empty
  // archive" from "haven't looked yet").
  hasLoaded: boolean
  error: string | null
}

const initialState: VodsState = {
  sessions: [],
  unavailableRoots: [],
  daemonScanning: false,
  loading: false,
  progress: null,
  hasLoaded: false,
  error: null,
}

// The listing lives at module scope, not in a hook, because more than one
// screen reads it (the VOD Archive and the dashboard's VOD-count column) and
// screens unmount on navigation. Per-hook state would re-download the whole
// archive on every visit, and twice over for two screens showing the same data.
let store: VodsState = initialState
const listeners = new Set<() => void>()

function subscribe(cb: () => void): () => void {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

function snapshot(): VodsState {
  return store
}

function update(fn: (s: VodsState) => VodsState) {
  const next = fn(store)
  if (next === store) return
  store = next
  for (const cb of listeners) cb()
}

// Rev of the last fully-downloaded listing; "" = nothing yet, which
// fetchRecordings treats as "always re-pull".
let rev = ""
// Load sequence token: a newer load invalidates every older one's updates
// (focus + nudge can race; last call wins, no interleaved pages).
let seq = 0
// Consumers currently suspending the nudge- and focus-driven reloads. Held as
// a set rather than a flag so one screen's bulk delete isn't un-paused by
// another screen that never asked for a pause.
const pausers = new Set<object>()
// A nudge swallowed while paused still has to be answered once, for a caller
// that pauses without driving its own refresh afterwards.
let missedNudge = false

// refresh pulls the listing progressively (the first page renders while the
// rest downloads) and skips re-downloading when the daemon's content rev hasn't
// moved — the common case, since nudges also fire for rescans that found
// nothing and for Scanning-flag settling.
//
// rescan (manual Refresh) forces a daemon filesystem re-walk and implies a full
// re-download; force (e.g. post-delete) just bypasses the rev probe.
export async function refreshRecordings(opts?: {
  force?: boolean
  rescan?: boolean
}): Promise<void> {
  const mySeq = ++seq
  update((s) => ({ ...s, loading: true }))
  const force = opts?.rescan || opts?.force
  try {
    const res = await fetchRecordings(
      (offset, limit, rescan) =>
        getDaemonClient().listVodsPage(offset, limit, rescan),
      {
        knownRev: force ? "" : rev,
        rescan: opts?.rescan,
        onPage: (p) => {
          if (mySeq !== seq) return
          update((s) => ({
            ...s,
            sessions: p.sessions,
            unavailableRoots: p.unavailableRoots,
            daemonScanning: p.scanning,
            progress: p.done
              ? null
              : { loaded: p.sessions.length, total: p.total },
          }))
        },
      },
    )
    if (mySeq !== seq) return
    rev = res.rev
    update((s) => ({
      ...s,
      // An unchanged probe means our sessions are still current; only the
      // volatile flags are fresh.
      ...(res.unchanged ? {} : { sessions: res.sessions }),
      unavailableRoots: res.unavailableRoots,
      daemonScanning: res.scanning,
      loading: false,
      progress: null,
      hasLoaded: true,
      error: null,
    }))
  } catch (e) {
    if (mySeq !== seq) return
    update((s) => ({
      ...s,
      loading: false,
      progress: null,
      error: e instanceof Error ? e.message : String(e),
    }))
  }
}

// removeSessions drops rows the caller has just deleted, so the grid settles
// one card at a time instead of jumping when the listing is re-pulled.
// Clearing the rev makes the next fetch a full re-download rather than a
// probe that would report our locally-edited listing as still current.
export function removeSessions(dirs: string[]) {
  if (dirs.length === 0) return
  const gone = new Set(dirs)
  rev = ""
  update((s) => ({ ...s, sessions: s.sessions.filter((v) => !gone.has(v.dir)) }))
}

function onExternalChange() {
  if (pausers.size > 0) {
    missedNudge = true
    return
  }
  void refreshRecordings()
}

// One daemon watch and one focus listener for however many screens are
// mounted, established on the first and torn down with the last.
let watchers = 0
let stopWatching: (() => void) | null = null

function addWatcher() {
  if (++watchers > 1) return
  const offVods = getDaemonClient().onVodsChanged(onExternalChange)
  window.addEventListener("focus", onExternalChange)
  stopWatching = () => {
    offVods()
    window.removeEventListener("focus", onExternalChange)
  }
}

function removeWatcher() {
  if (--watchers > 0) return
  stopWatching?.()
  stopWatching = null
}

function releasePause(token: object) {
  if (!pausers.delete(token)) return
  if (pausers.size > 0 || !missedNudge) return
  missedNudge = false
  void refreshRecordings({ force: true })
}

// useRecordings subscribes a screen to the shared listing and keeps it loaded:
// on mount (once connected), on daemon nudges and on window focus. All of
// these are rev-conditional, so unchanged content costs one tiny probe.
//
// paused suspends the nudge- and focus-driven reloads (not refreshRecordings,
// which the caller still drives explicitly). A caller mutating the archive
// itself — a bulk delete — knows what it changed, and re-downloading the whole
// listing after every one of its own writes only competes with them for the
// connection while the screen churns underneath the user.
export function useRecordings(
  connected: boolean,
  opts?: { paused?: boolean },
): VodsState & {
  refresh: (opts?: { force?: boolean; rescan?: boolean }) => Promise<void>
  removeSessions: (dirs: string[]) => void
} {
  const state = useSyncExternalStore(subscribe, snapshot, snapshot)

  const paused = opts?.paused ?? false
  const pauseToken = useRef<object>(null)
  if (pauseToken.current === null) pauseToken.current = {}
  useEffect(() => {
    const token = pauseToken.current as object
    if (!paused) {
      releasePause(token)
      return
    }
    pausers.add(token)
    // Discard any load already in flight: its pages predate the caller's
    // edits and would put the rows it removed back on screen.
    seq++
    update((s) => (s.loading ? { ...s, loading: false, progress: null } : s))
    return () => releasePause(token)
  }, [paused])

  useEffect(() => {
    if (!connected) return
    void refreshRecordings()
    addWatcher()
    return removeWatcher
  }, [connected])

  return { ...state, refresh: refreshRecordings, removeSessions }
}

// vodCountKey is how an archived session is matched to a streamer. Usernames
// are unique within a platform, but the folder a session was written to can
// differ in case from the tracked streamer, so both halves are lowered.
export function vodCountKey(platform: string, username: string): string {
  return `${platform.toLowerCase()}/${username.toLowerCase()}`
}

// useVodCounts reports how many archived sessions each streamer has, keyed by
// vodCountKey. It rides the same shared listing as the VOD Archive, so the
// count column costs no extra download.
//
// Only a settled listing is counted. The daemon serves its cached listing
// immediately and reconciles against the filesystem in the background, and a
// large listing arrives one page at a time — counting either would print
// numbers that climb while the user reads them. hasLoaded says whether a
// settled listing has arrived at all, so "not counted yet" and "no VODs" are
// distinguishable; after the first one the previous counts are held rather than
// blanked, so a routine rescan doesn't empty the column.
export function useVodCounts(connected: boolean): {
  counts: Map<string, number>
  hasLoaded: boolean
} {
  const { sessions, hasLoaded, daemonScanning, progress } =
    useRecordings(connected)
  const settled = hasLoaded && !daemonScanning && progress === null
  const [counted, setCounted] = useState<VOD[] | null>(null)
  useEffect(() => {
    if (settled) setCounted(sessions)
  }, [settled, sessions])

  const counts = useMemo(() => {
    const m = new Map<string, number>()
    for (const s of counted ?? []) {
      const key = vodCountKey(s.platform, s.username)
      m.set(key, (m.get(key) ?? 0) + 1)
    }
    return m
  }, [counted])
  return { counts, hasLoaded: counted !== null }
}

import {
  getDaemonClient,
  type PlatformLoginView,
  type ConnectionInfo,
  type DaemonMeta,
  type DiskStatus,
  type EncodeState,
  type PlatformInfo,
  type RecordingStats,
  type StreamersSnapshot,
  type StreamerView,
} from "@/lib/daemon"
import { registerPlatforms } from "@/data/supported-platforms"

export interface DaemonSlice {
  connection: ConnectionInfo
  // streamers is the structural streamer list with live stats stripped. Its
  // array (and per-row object) identity is preserved across stats-only pushes,
  // so the dashboard table only re-renders when membership/status/options
  // actually change, not on every recording stats tick.
  streamers: StreamerView[]
  // statsById holds the volatile per-recording stats keyed by "platform/username".
  // Components that show live stats subscribe to their own entry so a stats tick
  // re-renders just that leaf, not the table.
  statsById: Record<string, RecordingStats | null>
  daemon: DaemonMeta
  // accounts is the user's third-party platform logins (Accounts page). Its
  // array identity is preserved across unrelated pushes so the Accounts table
  // doesn't re-render on streamer stats ticks.
  accounts: PlatformLoginView[]
  // encode is the encode queue's live state (rev marker + running-job
  // progress); identity preserved across unrelated pushes.
  encode: EncodeState
  platforms: PlatformInfo[]
  daemonReady: boolean
  initDaemon: () => Promise<void>
  retryConnect: () => Promise<void>
}

const EMPTY_DAEMON: DaemonMeta = {
  version: null,
  recommendedVersion: null,
  updateRequired: false,
  blocked: false,
  blockedUntil: null,
  service: null,
  startTime: null,
  subscriptionCount: 0,
  workerCount: 0,
  serverVersion: null,
  serverStartedAt: null,
  diskStatus: null,
  registration: null,
}

function streamerId(s: { platform: string; username: string }): string {
  return `${s.platform}/${s.username}`
}

// structuralEqual compares two views on everything except live stats, so a
// reused row keeps its identity (and the table its render) across stats ticks.
function structuralEqual(a: StreamerView, b: StreamerView): boolean {
  return (
    a.platform === b.platform &&
    a.username === b.username &&
    a.displayName === b.displayName &&
    a.avatarUrl === b.avatarUrl &&
    a.liveStatus === b.liveStatus &&
    a.monitor === b.monitor &&
    a.record === b.record &&
    a.variant === b.variant &&
    a.outputTemplate === b.outputTemplate &&
    a.deletePartFiles === b.deletePartFiles &&
    a.splitSize === b.splitSize &&
    a.splitDuration === b.splitDuration &&
    a.activity === b.activity &&
    a.addedAt === b.addedAt &&
    a.lastLiveStart === b.lastLiveStart &&
    a.lastLiveDurationSec === b.lastLiveDurationSec
  )
}

// reconcileStreamers projects the snapshot to a stats-free structural list,
// reusing prior row objects (and the prior array itself) when nothing
// structural changed. The daemon emits a deterministic sort order, so a
// position-wise walk is sufficient to detect add/remove/reorder.
function reconcileStreamers(
  prev: StreamerView[],
  next: StreamerView[],
): StreamerView[] {
  const prevById = new Map(prev.map((s) => [streamerId(s), s]))
  let changed = prev.length !== next.length
  const result = next.map((s) => {
    const stripped = s.stats === null ? s : { ...s, stats: null }
    const old = prevById.get(streamerId(s))
    if (old && structuralEqual(old, stripped)) return old
    changed = true
    return stripped
  })
  return changed ? result : prev
}

function buildStatsById(
  streamers: StreamerView[],
): Record<string, RecordingStats | null> {
  const byId: Record<string, RecordingStats | null> = {}
  for (const s of streamers) byId[streamerId(s)] = s.stats
  return byId
}

function accountsEqual(a: PlatformLoginView[], b: PlatformLoginView[]): boolean {
  if (a.length !== b.length) return false
  return a.every((x, i) => {
    const y = b[i]
    return (
      x.platform === y.platform &&
      x.account === y.account &&
      x.expiresHint === y.expiresHint &&
      x.capturedAt === y.capturedAt &&
      x.cookieCount === y.cookieCount &&
      x.healthy === y.healthy &&
      x.lastCheckedAt === y.lastCheckedAt
    )
  })
}

const EMPTY_ENCODE: EncodeState = { queueRev: null, pending: 0, progress: null }

function encodeEqual(a: EncodeState, b: EncodeState): boolean {
  return (
    a.queueRev === b.queueRev &&
    a.pending === b.pending &&
    a.progress?.job_id === b.progress?.job_id &&
    a.progress?.stage === b.progress?.stage &&
    a.progress?.percent === b.progress?.percent &&
    a.progress?.eta_sec === b.progress?.eta_sec &&
    a.progress?.speed === b.progress?.speed
  )
}

function daemonMetaEqual(a: DaemonMeta, b: DaemonMeta): boolean {
  return (
    a.version === b.version &&
    a.recommendedVersion === b.recommendedVersion &&
    a.startTime === b.startTime &&
    a.subscriptionCount === b.subscriptionCount &&
    a.workerCount === b.workerCount &&
    diskStatusEqual(a.diskStatus, b.diskStatus)
  )
}

// Compares what the storage bars and the low-space banner actually render.
//
// Deliberately ignores sampled_at: the daemon re-samples on a timer, so
// including it would report every sample as a change. Every rendered field must
// be compared, or a change to it alone goes unnoticed.
function diskStatusEqual(a: DiskStatus | null, b: DiskStatus | null): boolean {
  if (a === b) return true
  if (!a || !b) return false
  if (a.warn_bytes !== b.warn_bytes) return false
  if (a.roots.length !== b.roots.length) return false
  return a.roots.every((r, i) => {
    const o = b.roots[i]
    return (
      r.state === o.state &&
      r.download === o.download &&
      r.error === o.error &&
      r.usage.free_bytes === o.usage.free_bytes &&
      r.usage.total_bytes === o.usage.total_bytes &&
      r.paths.length === o.paths.length &&
      r.paths.every((p, j) => p === o.paths[j])
    )
  })
}

export const createDaemonSlice = (set: any, get: any): DaemonSlice => {
  // ingest splits an incoming snapshot into its stable structural list, the
  // volatile stats map, and daemon meta, updating only the parts that changed.
  const ingest = (snapshot: StreamersSnapshot) => {
    const prev = get()
    set({
      streamers: reconcileStreamers(prev.streamers, snapshot.streamers),
      statsById: buildStatsById(snapshot.streamers),
      daemon: daemonMetaEqual(prev.daemon, snapshot.daemon)
        ? prev.daemon
        : snapshot.daemon,
      accounts: accountsEqual(prev.accounts, snapshot.accounts)
        ? prev.accounts
        : snapshot.accounts,
      encode:
        snapshot.encode == null || encodeEqual(prev.encode, snapshot.encode)
          ? prev.encode
          : snapshot.encode,
    })
  }

  return {
    connection: { state: "connecting" },
    streamers: [],
    statsById: {},
    daemon: EMPTY_DAEMON,
    accounts: [],
    encode: EMPTY_ENCODE,
    platforms: [],
    daemonReady: false,
    initDaemon: async () => {
      if (get().daemonReady) return
      set({ daemonReady: true })
      const client = getDaemonClient()

      // Store the platform list and register its stylized display names and
      // live-page templates so platformDisplayName / streamerChannelURL resolve
      // them everywhere.
      const setPlatforms = (platforms: PlatformInfo[]) => {
        registerPlatforms(platforms)
        set({ platforms })
      }

      client.onConnectionChanged((connection) => {
        set({ connection })
        if (connection.state === "connected") {
          client
            .getPlatforms()
            .then(setPlatforms)
            .catch(() => {})
        }
      })
      client.onStreamersChanged((snapshot) => ingest(snapshot))

      try {
        const [connection, snapshot] = await Promise.all([
          client.getConnection(),
          client.getSnapshot(),
        ])
        set({ connection })
        ingest(snapshot)
      } catch (e) {
        console.error("initDaemon initial fetch failed:", e)
      }
      try {
        setPlatforms(await client.getPlatforms())
      } catch {
        // Daemon not connected yet; platforms are refetched on connect.
      }
    },
    retryConnect: async () => {
      try {
        set({ connection: await getDaemonClient().retryConnect() })
      } catch (e) {
        console.error("retryConnect failed:", e)
      }
    },
  }
}

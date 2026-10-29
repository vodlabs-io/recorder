import type {
  PlatformLoginView,
  Activity,
  DiskStatus,
  DaemonMeta,
  EncodeProgress,
  EncodeState,
  LiveStatus,
  RecordingStats,
  RegistrationState,
  ServiceState,
  StreamersSnapshot,
  StreamerView,
} from "../types";
import {
  ENCODE_PENDING_KEY,
  ENCODE_PROGRESS_KEY,
  ENCODE_QUEUE_REV_KEY,
} from "./subjects";

// AccountDoc mirrors platformauth.Status: the non-secret status of a stored
// platform login, stored as JSON under `platformlogin.<platform>.state`. NO
// cookie values are ever present here.
type AccountDoc = {
  platform?: string;
  account?: string;
  expires_hint?: number;
  captured_at?: number;
  cookie_count?: number;
  healthy?: boolean;
  last_checked_at?: number;
};

// StreamerDoc mirrors vlstate.StreamerDoc: the daemon's consolidated
// per-streamer view document, stored as JSON under `streamer.<p>.<u>.state`.
// A doc's existence is what gives a streamer a dashboard row; the daemon owns
// all derivation (activity, one-shot visibility, effective options).
type StreamerDoc = {
  schema: number;
  platform?: string;
  username?: string;
  status?: LiveStatus;
  last_live_start?: number;
  last_live_duration?: number;
  live_metadata?: { title?: string; display_name?: string };
  display_name?: string;
  added_at?: number;
  avatar_url?: string;
  monitor: boolean;
  record: boolean;
  variant?: string;
  output_template?: string;
  delete_part_files?: boolean | null;
  split_size?: string;
  split_duration?: string;
  archive_dir?: string;
  activity?: Activity;
  active_session_id?: string;
};

// Raw recording.Stats from KV `session.<id>.stats`.
type RawStats = {
  total_bytes?: number;
  duration_ns?: number;
  speed_bps?: number;
};

/**
 * StreamerState caches the daemon's live KV stream (view docs, session stats,
 * daemon counters) and renders it into the snapshot pushed to the webview.
 */
export class StreamerState {
  private kv = new Map<string, string>();

  /**
   * Applies a single KV watch entry. value is undefined for DEL/PURGE. Returns
   * whether the stored state actually changed, so callers can skip a redundant
   * snapshot push when the daemon re-writes a key with its existing value (which
   * the daemon does continuously, e.g. heartbeat/counter keys).
   */
  applyKvEntry(
    key: string,
    op: "PUT" | "DEL" | "PURGE",
    value?: string,
  ): boolean {
    if (op === "PUT" && value !== undefined) {
      if (this.kv.get(key) === value) return false;
      this.kv.set(key, value);
      return true;
    }
    if (!this.kv.has(key)) return false;
    this.kv.delete(key);
    return true;
  }

  /** Drops all KV state (e.g. on disconnect) so stale rows don't linger. */
  clearKv(): void {
    this.kv.clear();
  }

  /**
   * Drops every cached KV key not present in `keep`. Used after reconnecting to
   * a daemon that may have rebuilt its store: keys it no longer has would
   * otherwise linger forever (no DEL ever arrives for them) and keep resurrecting
   * stale rows. Returns whether anything changed.
   */
  retainKv(keep: Set<string>): boolean {
    let changed = false;
    for (const key of this.kv.keys()) {
      if (!keep.has(key)) {
        this.kv.delete(key);
        changed = true;
      }
    }
    return changed;
  }

  snapshot(): StreamersSnapshot {
    const daemon: DaemonMeta = {
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
    };
    const docs: StreamerDoc[] = [];
    const statsBySession = new Map<string, RecordingStats>();
    const accounts: PlatformLoginView[] = [];
    const encode: EncodeState = { queueRev: null, pending: 0, progress: null };

    for (const [key, val] of this.kv) {
      if (key.startsWith("daemon.")) {
        this.applyDaemonKey(daemon, key, val);
        continue;
      }
      if (key.startsWith("platformlogin.")) {
        const acc = parseAccount(val);
        if (acc) accounts.push(acc);
        continue;
      }
      if (key.startsWith("encode.")) {
        if (key === ENCODE_QUEUE_REV_KEY) {
          encode.queueRev = val;
        } else if (key === ENCODE_PENDING_KEY) {
          const n = Number.parseInt(val, 10);
          encode.pending = Number.isFinite(n) && n > 0 ? n : 0;
        } else if (key === ENCODE_PROGRESS_KEY) {
          encode.progress = parseEncodeProgress(val);
        }
        continue;
      }
      const parts = key.split(".");
      if (parts[0] !== "streamer") continue;
      if (parts.length === 4 && parts[3] === "state") {
        const doc = parseDoc(val);
        if (doc) docs.push(doc);
      } else if (
        parts.length === 6 &&
        parts[3] === "session" &&
        parts[5] === "stats"
      ) {
        const stats = parseStats(val);
        if (stats) statsBySession.set(parts[4], stats);
      }
    }
    accounts.sort((a, b) => a.platform.localeCompare(b.platform));

    const streamers = docs.map((doc): StreamerView => {
      const sid = doc.active_session_id;
      return {
        platform: doc.platform ?? "",
        username: doc.username ?? "",
        displayName: doc.display_name || undefined,
        avatarUrl: doc.avatar_url || undefined,
        liveStatus: doc.status ?? "unknown",
        monitor: doc.monitor,
        record: doc.record,
        variant: doc.variant ?? "",
        outputTemplate: doc.output_template ?? "",
        deletePartFiles: doc.delete_part_files ?? null,
        splitSize: doc.split_size ?? "",
        splitDuration: doc.split_duration ?? "",
        archiveDir: doc.archive_dir ?? "",
        activity: doc.activity ?? "idle",
        stats: (sid && statsBySession.get(sid)) || null,
        addedAt: doc.added_at ?? null,
        lastLiveStart: doc.last_live_start ?? null,
        lastLiveDurationSec: doc.last_live_duration ?? null,
      };
    });
    streamers.sort((a, b) => sortKey(a).localeCompare(sortKey(b)));
    return { streamers, daemon, accounts, encode };
  }

  private applyDaemonKey(meta: DaemonMeta, key: string, val: string): void {
    switch (key) {
      case "daemon.version":
        meta.version = val;
        break;
      case "daemon.start_time":
        meta.startTime = toInt(val) ?? null;
        break;
      case "daemon.subscription_count":
        meta.subscriptionCount = toInt(val) ?? 0;
        break;
      case "daemon.worker_count":
        meta.workerCount = toInt(val) ?? 0;
        break;
      case "daemon.disk_status":
        try {
          meta.diskStatus = JSON.parse(val) as DiskStatus;
        } catch {
          /* ignore malformed disk status */
        }
        break;
      case "daemon.registration":
        try {
          meta.registration = JSON.parse(val) as RegistrationState;
        } catch {
          /* ignore malformed registration state */
        }
        break;
      case "daemon.service":
        try {
          meta.service = JSON.parse(val) as ServiceState;
        } catch {
          /* ignore malformed service state */
        }
        break;
      case "daemon.upstream_version":
        try {
          // Keys match version.Upstream's Go JSON tags.
          const upstream = JSON.parse(val) as {
            update_required?: boolean;
            blocked?: boolean;
            blocked_until?: number;
            recommended_version?: string;
            server_version?: string;
            server_started_at?: number;
          };
          meta.updateRequired = upstream.update_required ?? false;
          meta.blocked = upstream.blocked ?? false;
          meta.blockedUntil = upstream.blocked_until || null;
          meta.recommendedVersion = upstream.recommended_version ?? null;
          meta.serverVersion = upstream.server_version ?? null;
          meta.serverStartedAt = upstream.server_started_at ?? null;
        } catch {
          /* ignore malformed upstream version */
        }
        break;
    }
  }
}

function sortKey(v: StreamerView): string {
  // "~" sorts a doc that names nobody — malformed, since the daemon writes one
  // only for a resolved streamer — last rather than first.
  return v.platform || v.username ? `${v.platform}/${v.username}` : "~";
}

function parseDoc(val: string): StreamerDoc | undefined {
  try {
    return JSON.parse(val) as StreamerDoc;
  } catch {
    return undefined;
  }
}

function parseAccount(val: string): PlatformLoginView | undefined {
  try {
    const raw = JSON.parse(val) as AccountDoc;
    if (!raw.platform) return undefined;
    return {
      platform: raw.platform,
      account: raw.account || undefined,
      expiresHint: raw.expires_hint ?? 0,
      capturedAt: raw.captured_at ?? 0,
      cookieCount: raw.cookie_count ?? 0,
      healthy: raw.healthy ?? false,
      lastCheckedAt: raw.last_checked_at ?? 0,
    };
  } catch {
    return undefined;
  }
}

function parseEncodeProgress(val: string): EncodeProgress | null {
  try {
    const raw = JSON.parse(val) as EncodeProgress;
    if (typeof raw.job_id !== "number") return null;
    return raw;
  } catch {
    return null;
  }
}

function parseStats(val: string): RecordingStats | undefined {
  try {
    const raw = JSON.parse(val) as RawStats;
    return {
      totalBytes: raw.total_bytes ?? 0,
      durationMs: Math.round((raw.duration_ns ?? 0) / 1e6),
      speed: raw.speed_bps ?? 0,
    };
  } catch {
    return undefined;
  }
}

function toInt(val: string): number | undefined {
  const n = Number.parseInt(val, 10);
  return Number.isNaN(n) ? undefined : n;
}

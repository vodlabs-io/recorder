// Shared wire types for every client surface (desktop bun process, desktop
// webview, Web UI). Pure data types only — no runtime imports and no
// platform (electrobun/bun/browser) dependencies. The desktop-only RPC schema
// built on these lives in desktop/src/rpc.ts.

// LiveStatus mirrors the daemon's streamer.Status (the `status` field of the
// view doc in KV `streamer.<p>.<u>.state`).
export type LiveStatus =
  | "unknown"
  // "invalid" is an account never verified as valid. It is the reason an add
  // was rejected, not a state a tracked streamer holds — such a streamer is
  // never kept, so this should not reach a row.
  | "invalid"
  // "unavailable" is an account that WAS valid and is not reachable now:
  // deleted, banned, deactivated, or withheld behind a private or age gate.
  // Still checked, on a slow cadence, because the state is often temporary.
  | "unavailable"
  // "defunct" is an account that stayed unavailable long enough that the cloud
  // stopped checking it. The row stays; only the polling ends.
  | "defunct"
  | "removed"
  | "restricted"
  | "auth_required"
  | "offline"
  | "paused"
  | "live";

// Activity mirrors the daemon-derived activity in the view doc (vlstate
// Activity* constants): the active session state, post-processing, or the
// resting state from the monitor option. "paused" is the streamer pausing
// their own live — distinct from the user disabling recording for them.
export type Activity =
  | "recording"
  | "lagging"
  | "paused"
  | "processing"
  | "moving"
  | "moving_retrying"
  | "monitoring"
  | "idle"
  // Live and set to record, but the download folder has no room. Outranks every
  // other activity: a Broadcast is being lost right now.
  | "blocked_no_space";

// RecordingStats is the live progress of an active Session.
export type RecordingStats = {
  totalBytes: number;
  durationMs: number;
  speed: number; // bytes per second
};

// StreamerView is one row in the dashboard, mapped 1:1 from the daemon's
// per-streamer view doc (KV `streamer.<p>.<u>.state`).
export type StreamerView = {
  platform: string;
  username: string;
  // Human-facing label for platforms whose username is an opaque id. Mutable
  // and not unique, so it is shown and never keyed on — identity, filters and
  // output paths all stay on username. Undefined means show the username.
  displayName?: string;
  avatarUrl?: string; // re-hosted avatar image URL, if the cloud has one
  liveStatus: LiveStatus;
  monitor: boolean;
  record: boolean; // false = user switched recording off; still monitored
  variant: string; // "" = inherit global default
  outputTemplate: string; // "" = inherit global default
  deletePartFiles: boolean | null; // null = inherit global default
  splitSize: string; // "" = inherit global default; "off" = disabled
  splitDuration: string; // "" = inherit global default; "off" = disabled
  archiveDir: string; // "" = inherit global default; "off" = keep in downloads
  activity: Activity;
  stats: RecordingStats | null;
  addedAt: number | null; // unix seconds; when the streamer was first added
  lastLiveStart: number | null; // unix seconds
  lastLiveDurationSec: number | null;
};

export type ConnectionState =
  | "connecting"
  // First-run bootstrap: the daemon binary is being downloaded from GitHub.
  | "downloading"
  | "connected"
  | "disconnected"
  | "no-binary";

export type ConnectionInfo = {
  state: ConnectionState;
  message?: string;
};

// QuitPrompt is pushed (and pullable) when the user asks to quit while the
// daemon still has work in flight: the recording/processing counts the quit
// dialog reports before the daemon is gracefully shut down.
export type QuitPrompt = {
  recording: number;
  processing: number;
};

// ServiceState is the daemon's view of the VodLabs service. While it is
// unreachable nothing is checked, so no new recording can start; the ones
// already running carry on.
export type ServiceState = {
  reachable: boolean;
  error?: string;
  since: string;
  updated_at: string;
};

export type DaemonMeta = {
  version: string | null;
  recommendedVersion: string | null;
  // True when the service will no longer serve this daemon version: every call
  // but the heartbeat is refused until it updates.
  updateRequired: boolean;
  // Whether the daemon can reach the VodLabs service (rides daemon.service);
  // null until the daemon has written it.
  service: ServiceState | null;
  startTime: number | null; // unix seconds
  subscriptionCount: number;
  workerCount: number;
  serverVersion: string | null; // backend build (from heartbeat)
  serverStartedAt: number | null; // backend start, unix seconds
  // Free space per Storage root (rides the daemon.disk_status KV key); null
  // until the daemon has sampled.
  diskStatus: DiskStatus | null;
  // Whether the daemon has an identity yet (rides the daemon.registration KV
  // key); null until the daemon has written it.
  registration: RegistrationState | null;
};

// RegistrationState mirrors the daemon's registration KV doc. A daemon needs an
// identity before it can ask the API anything, and obtaining one is the single
// startup step with no fallback — so a first run without a working connection
// reports itself here rather than looking like an install that does nothing.
export type RegistrationState = {
  registered: boolean;
  // The last failure, so the UI can say why instead of sending the user to the
  // logs. Absent once registered.
  error?: string;
  attempts?: number;
  updated_at: string;
};

// SpaceState is a Storage root's space situation.
//
// "unknown" is not zero free space: the root could not be probed at all (an
// offline NAS, an unmounted drive). It needs a different message and a
// different response than a root that is genuinely full.
export type SpaceState = "ok" | "low" | "full" | "unknown";

// StorageRoot mirrors diskspace.Root: one distinct filesystem the daemon writes
// to, reached through one or more download or archive folders.
export type StorageRoot = {
  // The configured folders resolving to this root. Several folders share a root
  // when they are on the same disk; the first names it in the UI.
  paths: string[];
  // Whether the download folder is one of paths. This root losing space stops
  // recordings, so it is treated differently from an archive-only root.
  download: boolean;
  usage: { total_bytes: number; free_bytes: number };
  state: SpaceState;
  // Why the root could not be probed. Present only when state is "unknown".
  error?: string;
};

// DiskStatus mirrors the daemon's disk_status KV doc.
export type DiskStatus = {
  roots: StorageRoot[];
  // The level the states were classified against, so a client can explain the
  // warning it shows without reading config separately.
  warn_bytes: number;
  sampled_at: string;
};

// PlatformLoginView mirrors the daemon's platformlogin.<platform>.state KV doc
// (platformauth.Status): the NON-SECRET status of a stored third-party platform
// login. It carries NO cookie values — only what the Accounts table renders.
// PRIVACY: it carries no cookie values, and the KV it rides never leaves this
// machine.
export type PlatformLoginView = {
  platform: string;
  account?: string;
  expiresHint: number; // unix seconds, 0 = unknown
  capturedAt: number; // unix seconds
  cookieCount: number;
  healthy: boolean;
  lastCheckedAt: number; // unix seconds, 0 = never checked
};

export type StreamersSnapshot = {
  streamers: StreamerView[];
  daemon: DaemonMeta;
  accounts: PlatformLoginView[];
  // Encode queue live state (rides the local-only encode.> KV prefix; never
  // bridged to the Dashboard).
  encode: EncodeState;
};

// EncodeState is the snapshot's encode section: the queue's revision marker
// (re-fetch the listing when it moves), how many jobs are running or waiting,
// and the running job's live progress.
export type EncodeState = {
  queueRev: string | null;
  // Running + queued. 0 when the daemon hasn't published a count yet.
  pending: number;
  progress: EncodeProgress | null;
};

// EncodeProgress mirrors vlstate.EncodeProgress (the encode.progress KV key).
export type EncodeProgress = {
  job_id: number;
  stage: "encode" | "copy";
  // 0-100, or -1 when indeterminate (unknown total duration).
  percent: number;
  eta_sec?: number;
  speed?: number;
};

// EncodeJob mirrors encoding.Job (the encode.* endpoints): one job in the
// merge/re-encode queue.
export type EncodeJob = {
  id: number;
  type: string; // "merge" (future task types will add values)
  source: string; // "manual" (future: "auto")
  dir: string;
  platform?: string;
  username?: string;
  session_id?: string;
  inputs: string[];
  output: string;
  delete_parts: boolean;
  encoder?: string; // snapshotted preference; "" = auto
  position: number;
  status: "queued" | "running" | "done" | "failed" | "cancelled";
  stage?: "" | "encode" | "copy" | "finalize";
  error?: string;
  warning?: string;
  log_path?: string;
  created_at: number; // unix seconds
  started_at?: number;
  completed_at?: number;
};

// EncodeQueue mirrors encoding.Queue (encode.list).
export type EncodeQueue = {
  jobs: EncodeJob[];
  paused: boolean;
};

// EncoderStatus mirrors encoding.EncoderStatus: one H.264 encoder candidate on
// the daemon's machine (Settings → encoder dropdown).
export type EncoderStatus = {
  id: string;
  name: string;
  available: boolean; // compiled into the embedded ffmpeg build
  working: boolean; // passed a real self-test encode
  hardware: boolean;
};

// EncoderList mirrors vlapi.EncoderList (encode.encoders).
export type EncoderList = {
  selected: string; // "auto" | "software" | encoder id
  encoders: EncoderStatus[];
};

// SetPlatformLoginParams is the "advanced paste" path for storing a platform
// login from a raw Cookie header (Web UI + desktop + CLI). PRIVACY: the header
// is sent only to the LOCAL daemon; it never reaches the vodlabs cloud.
export type SetPlatformLoginParams = {
  platform: string;
  account?: string;
  cookieHeader: string;
};

// UserConfig mirrors vlapi.UserConfig (config.get / config.set).
export type UserConfig = {
  download_dir: string;
  variant: string;
  output_template: string;
  delete_part_files: boolean;
  split_size: string; // "" = no splitting
  split_duration: string; // "" = no splitting
  archive_dir: string; // "" = keep VODs in the download folder
  // Read-only folders the VOD Archive also lists VODs from — an archive left on
  // a retired disk, a folder of imports. Never written to: no VOD is
  // moved into one, they carry no free-space warning, and a VOD in one cannot
  // be merged. Absolute and cleaned, so a listed VOD's `root` matches an entry
  // here by exact string comparison.
  scan_dirs: string[];
  // Video encoder for merge/re-encode jobs: "auto" (default), "software"
  // (libx264), or a specific encoder id (e.g. "h264_videotoolbox").
  encoder: string;
  // Free space in GiB below which a Storage root is reported as running low.
  // 0 disables the warning; the hard floors that refuse and stop recording are
  // not affected by it.
  low_space_warn_gb: number;
};

// VODDeleteResult mirrors vlapi.vodDeleteResult: one entry's outcome in a batch
// delete. Failures carry their reason rather than collapsing into a count, so
// the user is told which VODs survived and why (a VOD that started moving after
// it was selected is refused by design).
export type VODDeleteResult = {
  dir: string;
  deleted: boolean;
  error?: string;
};

// VODDeleteManyResponse mirrors vlapi.vodDeleteManyResponse.
export type VODDeleteManyResponse = {
  results: VODDeleteResult[];
  deleted: number;
  failed: number;
};

// VODCancelMovesResponse mirrors vlapi.vodCancelMovesResponse: how many queued
// moves were retired.
export type VODCancelMovesResponse = {
  cancelled: number;
};

// LoginFlow mirrors vlconfig.loginFlow: public metadata for signing in
// to a platform to record login-restricted streams. It carries NO cookie values
// — the login itself is captured by the desktop and stored only on the daemon.
export type LoginFlow = {
  url: string;
  // Page the background refresh reloads to re-mint the jar; falls back to `url`.
  refresh_url?: string;
  cookie_domains?: string[];
  success_url_pattern?: string;
  session_cookie?: string; // cookie whose presence means "signed in" (e.g. sessionid)
};

// PlatformInfo mirrors vlconfig.Platform. display_name is the stylized label
// (e.g. "TikTok") shown wherever the UI renders a platform name; when absent,
// callers capitalize `name`.
export type PlatformInfo = {
  name: string;
  display_name?: string;
  // Template for the streamer's live page on this platform, with {username}
  // substituted. When absent, no link is shown.
  channel_url?: string;
  login?: LoginFlow;
  // Set for a platform whose streamers can only be recorded once (pages on
  // sites without their own support): there is no channel to monitor.
  no_monitor?: boolean;
};

// CloudInterval mirrors vlapi.CloudInterval: the estimated polling interval for
// one platform. estimatedSeconds/updatedAt are null when no estimate exists yet.
export type CloudInterval = {
  platform: string;
  estimated_seconds: number | null;
  updated_at: number | null; // unix seconds
};

// CloudIntervals mirrors vlapi.CloudIntervals (the cloud.intervals endpoint).
export type CloudIntervals = {
  target_seconds: number;
  intervals: CloudInterval[];
};

// UpdateResult mirrors vlapi.UpdateResponse (the update endpoint). The daemon
// checks for a newer build and, when found, downloads it (or, in a container,
// returns instructions to pull the new image); `message` is human-readable.
export type UpdateResult = {
  daemon_version: string;
  message: string;
  updated: boolean;
  container: boolean;
};

// UpdatePhase is the per-component update lifecycle, shared by the desktop app
// (Electrobun Updater) and the daemon (a downloaded binary the bun process
// spawns). Both are owned by the bun process and pushed to the webview together
// in UpdateState via updateStateChanged.
//   idle        — not checked yet this session
//   checking    — a check is in flight
//   up-to-date  — checked; already on the newest version
//   downloading — a newer version was found and is downloading
//   ready       — downloaded; installUpdates() can install and restart
//   disabled    — updates don't apply here (dev build, container, path override)
//   error       — the check or download failed (see message)
export type UpdatePhase =
  | "idle"
  | "checking"
  | "up-to-date"
  | "downloading"
  | "ready"
  | "disabled"
  | "error";

// AppUpdateStatus is the desktop app's half of the Update tab.
export type AppUpdateStatus = {
  phase: UpdatePhase;
  currentVersion: string; // the running app version (from version.json)
  channel: string; // release channel (e.g. "canary"; "dev" => disabled)
  availableVersion: string | null; // the newer version when one was found
  progress: number | null; // 0–100 while downloading, else null
  message: string | null; // human-readable error/info for the error phase
};

// DaemonUpdateStatus is the daemon's half. availableVersion is the cloud's
// recommended_version (from the heartbeat, via the daemon.upstream_version KV
// key) — the single source of truth for both "an update exists" and which tag
// gets downloaded, so a bad release can be held back.
export type DaemonUpdateStatus = {
  phase: UpdatePhase;
  currentVersion: string;
  availableVersion: string | null;
  progress: number | null;
  message: string | null;
  // container daemons are owned by Docker: the binary can't be swapped in place,
  // so we never download and the tab shows `docker compose pull` instead.
  container: boolean;
};

// InstallPhase tracks the combined install sequence behind the single "Install
// and restart" button. The daemon goes first and the app update is abandoned if
// it fails, so the user is never left with a new app and a broken recorder.
//   idle              — nothing installing
//   restarting-daemon — daemon shutting down / new binary starting
//   installing-app    — Updater.applyUpdate(); the process exits and relaunches
//   error             — the sequence stopped early (see message)
export type InstallPhase =
  | "idle"
  | "restarting-daemon"
  | "installing-app"
  | "error";

// UpdateState is everything the Update tab renders, pushed as one unit so the
// app half, the daemon half and the install sequence can never disagree.
export type UpdateState = {
  app: AppUpdateStatus;
  daemon: DaemonUpdateStatus;
  install: { phase: InstallPhase; message: string | null };
};

// DaemonInfo mirrors vlapi.DaemonInfo (the info endpoint).
// StartAtLogin is whether the OS will start the recorder when the user logs
// in. `supported` is false where the app has no way to ask (an unknown
// platform, or a host that is not the desktop app at all).
export type StartAtLogin = {
  supported: boolean;
  enabled: boolean;
};

export type DaemonInfo = {
  version: string;
  // Commit the daemon was built from; empty in a build that was not stamped.
  commit?: string;
  os: string;
  arch: string;
  container: boolean;
  nats_url: string;
  cloud_api_url: string;
  cloud_nats_url: string;
  executable_path: string;
  config_file_path: string;
  streamers_file_path: string;
  download_dir: string;
  // Global archive folder; "" when archiving is off.
  archive_dir: string;
  daemon_log_file_path: string;
  session_log_file_path: string;
  processing_log_file_path: string;
  cloud_log_file_path: string;
  scraper_log_file_path: string;
};

// StreamerRef identifies a streamer by platform+username or by url.
export type StreamerRef = {
  platform?: string;
  username?: string;
  url?: string;
};

export type AddStreamerParams = StreamerRef & {
  monitor: boolean; // true → add (continuous); false → record_once (one-shot)
};

// AddStreamerResult mirrors vlapi.AddResponse: the canonical platform/username
// the streamer was tracked under. A URL add is why it is returned — the daemon
// resolves the pasted URL and discards it, so this is the only way the caller
// learns which streamer it just added.
export type AddStreamerResult = {
  platform: string;
  username: string;
  // Set instead of an identity when the daemon refused to monitor a streamer
  // that can only be recorded once; message is its explanation. The caller
  // offers to record it once.
  refused?: "monitor_unsupported";
  message?: string;
};

// MARK: VOD Archive

// VODState mirrors vodindex's VOD states. "recording" never
// reaches clients (live sessions are suppressed from the VOD Archive).
// "encoding" = a merge/re-encode job is queued or running for the session
// (its copy-back phase surfaces as "moving").
export type VODState = "ready" | "processing" | "moving" | "encoding";

// VODFile mirrors vodindex.File: one produced output file (vod.mp4 / vodN.mp4)
// of a session. Times are RFC3339 strings, omitted when unknown (folders
// without a manifest).
export type VODFile = {
  name: string;
  bytes: number;
  start?: string;
  end?: string;
  duration_sec?: number;
  has_thumb: boolean;
};

// VOD mirrors vodindex.VOD: one finished session folder, grouping
// every split file it holds. files is null for sessions still processing that
// haven't produced an output yet.
export type VOD = {
  dir: string;
  root: string;
  platform: string;
  username: string;
  session_id: string;
  start?: string;
  end?: string;
  total_bytes: number;
  duration_sec: number;
  state: VODState;
  // Why a move is not progressing (no_space, unreachable, permission, other).
  // Empty while a move is simply running.
  reason?: MoveFailureReason;
  from_manifest: boolean;
  files: VODFile[] | null;
  // Stream titles the platform reported while the session recorded, oldest
  // first. A long broadcast is usually retitled more than once, so this is a
  // timeline rather than one value. Absent for sessions recorded without a
  // metadata sidecar, and for platforms that report no title.
  titles?: VODTitle[];
};

// VODTitle mirrors vodindex.Title: one title a session ran under, and when the
// recorder first saw it.
export type VODTitle = {
  text: string;
  at?: string;
};

// MoveFailureReason mirrors vodmove.Reason.
export type MoveFailureReason =
  | "no_space"
  | "unreachable"
  | "permission"
  | "other";

// VODListPage mirrors vlapi's vods.list response (one page).
export type VODListPage = {
  sessions: VOD[];
  total: number;
  offset: number;
  unavailable_roots: string[] | null;
  scanned_at: string;
  // Content generation of the listing: unchanged across rescans that find
  // nothing new, so clients can skip re-downloading every page. A stringified
  // int64 (unix ns, beyond JS's safe-integer range) — compare for equality,
  // never ordering.
  rev?: string;
  // True when a background rescan is in flight and this listing may be stale or
  // (before the first scan completes) empty.
  scanning?: boolean;
};

// RecordingListResult is the client-side aggregation of every page.
export type RecordingListResult = {
  sessions: VOD[];
  unavailableRoots: string[];
  scannedAt: string;
  // Mirrors VODListPage.scanning: a refresh is in progress and results may be
  // stale/empty. Lets the UI show "scanning" rather than "no VODs".
  scanning: boolean;
};

// SetStreamerOptionsParams is the complete desired option state for a streamer.
// A null / omitted field clears that override (inherits the global default).
export type SetStreamerOptionsParams = StreamerRef & {
  monitor?: boolean | null;
  variant?: string | null;
  output_template?: string | null;
  delete_part_files?: boolean | null;
  split_size?: string | null; // null/omit = inherit; "off" = disable; "2GB" = override
  split_duration?: string | null; // null/omit = inherit; "off" = disable; "1h30m" = override
  archive_dir?: string | null; // null/omit = inherit; "off" = keep in downloads; "/mnt/nas" = override
};


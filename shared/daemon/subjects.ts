import type { StreamerRef } from "../types";

// Subjects are the daemon's NATS micro endpoints (see internal/vlapi/service.go).
// NATS micro subscribes endpoints on their (group-prefixed) name; the service
// name "APIService" is discovery metadata only and is NOT a subject prefix.
export const Subjects = {
  info: "info",
  update: "update",
  shutdown: "shutdown",
  shutdownForce: "shutdown_force",
  platforms: "platforms",
  cloudIntervals: "cloud.intervals",
  configGet: "config.get",
  configSet: "config.set",
  streamerList: "streamer.list",
  streamerAdd: "streamer.add",
  streamerRecordOnce: "streamer.record_once",
  streamerRemove: "streamer.remove",
  streamerRemoveAll: "streamer.remove_all",
  streamerRecordOff: "streamer.record_off",
  streamerRecordOffAll: "streamer.record_off_all",
  streamerRecordOn: "streamer.record_on",
  streamerRecordOnAll: "streamer.record_on_all",
  streamerCheck: "streamer.check",
  streamerSetOptions: "streamer.set_options",
  vodsList: "vods.list",
  vodsThumbnail: "vods.thumbnail",
  vodsDelete: "vods.delete",
  vodsDeleteMany: "vods.delete_many",
  vodsCancelMoves: "vods.cancel_moves",
  // Encode queue (VOD file merging / re-encode jobs). LOCAL-ONLY subjects:
  // deliberately absent from the Dashboard bridge's allowlist
  // (internal/cloud/bridge.go).
  encodeEnqueue: "encode.enqueue",
  encodeList: "encode.list",
  encodeRemove: "encode.remove",
  encodeReorder: "encode.reorder",
  encodeCancel: "encode.cancel",
  encodeRetry: "encode.retry",
  encodePause: "encode.pause",
  encodeResume: "encode.resume",
  encodeEncoders: "encode.encoders",
  // Third-party platform logins (Accounts UI). PRIVACY: local-only subjects,
  // never added to the Dashboard bridge's allowlist (internal/cloud/bridge.go).
  platformLoginSet: "platformlogin.set",
  platformLoginRemove: "platformlogin.remove",
  platformLoginList: "platformlogin.list",
  platformLoginHealth: "platformlogin.health",
} as const;

// PLATFORM_LOGIN_REFRESH_SUBJECT is a plain LOCAL NATS subject (not a micro endpoint)
// the daemon publishes on to ask a running desktop app to refresh a platform's
// login webview jar (see internal/platformlogin.RefreshRequestSubject). Payload:
// { platform: string }. PRIVACY: local-only, like every platformlogin.* subject.
export const PLATFORM_LOGIN_REFRESH_SUBJECT = "platformlogin.refresh";

// PLATFORM_BROWSERFETCH_SUBJECT is a LOCAL NATS request-reply subject the daemon
// uses to ask a running desktop app to execute one HTTP request inside a
// platform's login webview, so the platform's own in-page JS (e.g. TikTok's
// webmssdk) signs it (see internal/browserfetch). Request:
// { platform, url, method?, body?, timeout_ms? }; reply: { status, body?, error? }.
// PRIVACY: local-only — never bridged to the cloud; runs with the webview's own
// cookies.
export const PLATFORM_BROWSERFETCH_SUBJECT = "platform.browserfetch.request";

// PLATFORM_BROWSERFETCH_READY_SUBJECT announces to the daemon that a browser
// context now exists, published once per connection after subscribing above
// (see internal/browserfetch.ReadySubject), so checks the daemon set aside for
// want of one are asked again. Empty payload.
export const PLATFORM_BROWSERFETCH_READY_SUBJECT = "platform.browserfetch.ready";

// The KV key the daemon bumps when the VOD Archive's contents change (a VOD
// produced, moved, or deleted). It rides the existing "daemon.>" KV watch, so
// clients get change notifications without a dedicated subscription.
export const VODS_REV_KEY = "daemon.vods_rev";

// A VOD folder on a network drive can take seconds to remove, so this bounds
// one vods.delete_many call's request ceiling (the daemon's vodDeleteManyMax
// caps the batch size).
export const VODS_DELETE_TIMEOUT_MS = 120_000;

// A thumbnail whose sidecar .jpg is missing is generated on demand, which on a
// network drive is far slower than the default request ceiling, and a client
// retry would start another ffmpeg. The daemon bounds its own work slightly
// tighter (vodThumbnailTimeout), so a stuck generation reports an error.
export const VODS_THUMBNAIL_TIMEOUT_MS = 60_000;

// The KV key holding free space per Storage root (DiskStatus JSON). Rides the
// "daemon.>" watch like the keys above, so the desktop app, the Web UI and the
// Dashboard all see it without a dedicated subscription.
export const DISK_STATUS_KEY = "daemon.disk_status";

// The KV key holding whether the daemon has obtained its client identity yet
// (RegistrationState JSON). Only ever false before a daemon's first successful
// registration; it keeps a first run that cannot reach the API from looking
// like a broken install. Rides the "daemon.>" watch.
export const REGISTRATION_KEY = "daemon.registration";

// Encode-queue KV keys (local-only "encode.>" prefix — mirrored to LOCAL
// clients via DEFAULT_KV_FILTERS, never by the Dashboard bridge). queue_rev bumps
// whenever the queue's contents change; progress carries the running job's
// live EncodeProgress JSON.
export const ENCODE_QUEUE_REV_KEY = "encode.queue_rev";
export const ENCODE_PENDING_KEY = "encode.pending";
export const ENCODE_PROGRESS_KEY = "encode.progress";

// refPayload narrows a ref to a single identity form the daemon accepts
// (validateStreamerID rejects "both"). Resolved streamers carry both
// platform/username and url; prefer platform/username since that's how the
// daemon keys streamers.toml. Fall back to url for unresolved url-only entries.
export function refPayload(ref: StreamerRef): StreamerRef {
  if (ref.platform && ref.username) {
    return { platform: ref.platform, username: ref.username };
  }
  if (ref.url) return { url: ref.url };
  return {};
}

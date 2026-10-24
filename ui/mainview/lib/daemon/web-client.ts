import { wsconnect } from "@nats-io/nats-core"
import { DaemonSession } from "@shared/daemon/session"
import {
  Subjects,
  VODS_DELETE_TIMEOUT_MS,
  VODS_REV_KEY,
  VODS_THUMBNAIL_TIMEOUT_MS,
  refPayload,
} from "@shared/daemon/subjects"
import type {
  VODDeleteManyResponse,
  VODCancelMovesResponse,
  AddStreamerResult,
  VODListPage,
  CloudIntervals,
  ConnectionInfo,
  DaemonInfo,
  EncodeJob,
  EncodeQueue,
  EncoderList,
  FolderSuggestion,
  PlatformInfo,
  QuitPrompt,
  StartAtLogin,
  StreamersSnapshot,
  UpdateState,
  UserConfig,
} from "@shared/types"
import type { DaemonClient } from "./client"

// A few requests make the daemon wait on a remote answer before it can reply,
// so they need a ceiling above the daemon's own. Time out below it and the user
// gets a generic local timeout instead of the daemon's own account of what
// went wrong.
const CLOUD_ROUNDTRIP_TIMEOUT_MS = 20_000

// WebDaemonClient is the browser implementation of DaemonClient: it speaks
// NATS to the daemon over the WebSocket bridge the daemon's Web UI listener
// serves, driving the same DaemonSession engine the desktop bun process uses
// (KV mirroring, reconnect resync, debounced snapshots). Unlike the desktop, it
// never spawns or downloads a daemon — it can only connect to one that's
// already running — and it has no native dialogs, quit flow, or self-update
// (see capabilities).

export interface WebDaemonClientOptions {
  // The page's URL. The daemon's routes are resolved relative to it, so the
  // UI works under a reverse proxy's subpath.
  base: string
  name?: string
}

// WebUrls are the daemon routes the Web UI uses, resolved against the page.
export type WebUrls = {
  nats: string
  session: string
  signout: string
  signin: string
  media: (dir: string, file: string, download?: boolean) => string
}

export function webUrls(base: string): WebUrls {
  const at = (path: string) => new URL(path, base)
  const nats = at("nats")
  nats.protocol = nats.protocol === "https:" ? "wss:" : "ws:"
  return {
    nats: nats.href,
    session: at("api/session").href,
    signout: at("api/signout").href,
    signin: at("signin").href,
    media: (dir, file, download = false) => {
      const u = at("api/vods/file")
      u.searchParams.set("dir", dir)
      u.searchParams.set("file", file)
      if (download) u.searchParams.set("download", "1")
      return u.href
    },
  }
}

// sessionExpired reports whether the daemon says this browser is no longer
// signed in, as opposed to the daemon being unreachable.
export async function sessionExpired(
  sessionUrl: string,
  fetchFn: typeof fetch = fetch,
): Promise<boolean> {
  try {
    const res = await fetchFn(sessionUrl, {
      credentials: "same-origin",
      cache: "no-store",
    })
    return res.status === 401
  } catch {
    return false
  }
}

// The Web UI has no self-update: you update it by reloading the page.
const APP_UPDATE_UNAVAILABLE: UpdateState["app"] = {
  phase: "disabled",
  currentVersion: "",
  channel: "web",
  availableVersion: null,
  progress: null,
  message: null,
}

export class WebDaemonClient implements DaemonClient {
  readonly capabilities = {
    chooseDirectory: false,
    openPath: false,
    quit: false,
    appUpdate: false,
    // A browser owns neither the daemon's lifecycle nor its own build, so the
    // Update tab is read-only here: versions and availability, but no button.
    installUpdates: false,
    // A browser can't read another site's cookies, so webview login is
    // desktop-only; the Web UI offers the "paste a Cookie header" path instead.
    webviewLogin: false,
    // Signed in with the daemon's sign-in token, so it offers Sign out.
    webSession: true,
    // Recordings live on the daemon's machine; the daemon serves them.
    browserMedia: true,
  }

  private readonly session: DaemonSession
  private readonly urls: WebUrls
  // Whether the daemon runs in a container, from its info once connected.
  private container = false
  private connection: ConnectionInfo = { state: "connecting" }
  private readonly streamerListeners = new Set<(s: StreamersSnapshot) => void>()
  private readonly connectionListeners = new Set<(c: ConnectionInfo) => void>()
  private readonly vodsListeners = new Set<() => void>()

  constructor(opts: WebDaemonClientOptions) {
    this.urls = webUrls(opts.base)
    this.session = new DaemonSession({
      connect: wsconnect,
      connectOptions: {
        name: opts.name ?? "vodlabs-web",
      },
      events: {
        onSnapshot: (snap) => {
          for (const cb of this.streamerListeners) cb(snap)
        },
        onConnectionChanged: (info) => this.setConnection(info),
        // A dropped socket may be the daemon restarting, or this browser
        // being signed out (a rotated token, a sign-out elsewhere). Only the
        // latter needs the sign-in page.
        onDisconnect: () => void this.redirectIfSignedOut(),
        // The VOD-Archive-changed signal rides the daemon.vods_rev KV key, which
        // the session's existing "daemon.>" watch already mirrors.
        onKvEntryChanged: (key) => {
          if (key === VODS_REV_KEY) {
            for (const cb of this.vodsListeners) cb()
          }
        },
      },
    })
    // Pause snapshot delivery while the tab is hidden (browsers throttle
    // background tabs; the flush on becoming visible avoids a stale-stats
    // fast-forward, same as the desktop webview).
    document.addEventListener("visibilitychange", () =>
      this.session.setVisible(!document.hidden),
    )
    this.session.setVisible(!document.hidden)

    void this.ensureConnected()
  }

  private setConnection(info: ConnectionInfo): void {
    this.connection = info
    for (const cb of this.connectionListeners) cb(info)
  }

  private async redirectIfSignedOut(): Promise<boolean> {
    if (!(await sessionExpired(this.urls.session))) return false
    location.replace(this.urls.signin)
    return true
  }

  private async ensureConnected(): Promise<void> {
    this.setConnection({ state: "connecting" })
    try {
      await this.session.connect(this.urls.nats)
      await this.session.start()
      void this.getInfo()
        .then((info) => {
          this.container = info.container
        })
        .catch(() => {})
    } catch {
      if (await this.redirectIfSignedOut()) return
      this.setConnection({
        state: "disconnected",
        message: "Can't reach the recorder daemon. Is it still running?",
      })
    }
  }

  getConnection = () => Promise.resolve(this.connection)
  getSnapshot = () => Promise.resolve(this.session.snapshot())
  getPlatforms = () => this.session.request<PlatformInfo[]>(Subjects.platforms)
  getCloudIntervals = () =>
    this.session.request<CloudIntervals>(Subjects.cloudIntervals)
  getInfo = () => this.session.request<DaemonInfo>(Subjects.info)
  // A browser tab has no log file of its own; its console is the log.
  getDesktopLogPath = () => Promise.resolve("")

  getConfig = () => this.session.request<UserConfig>(Subjects.configGet)
  setConfig = (cfg: UserConfig) =>
    this.session.request<UserConfig>(Subjects.configSet, cfg)
  getFolderSuggestions = () =>
    this.session.request<FolderSuggestion[]>(Subjects.configFolders)

  // Platform logins. The browser can't open a native webview to read another
  // site's cookies (webviewLogin capability is false), so platformLogin is
  // unavailable here — the Accounts UI shows the paste path instead.
  // PRIVACY: the pasted cookie header goes only to the LOCAL daemon over local
  // NATS; it never reaches the vodlabs cloud.
  platformLogin = (): Promise<boolean> => {
    throw new Error("webview login is only available in the desktop app")
  }
  setPlatformCookies = async (
    params: Parameters<DaemonClient["setPlatformCookies"]>[0],
  ) => {
    await this.session.request(Subjects.platformLoginSet, {
      platform: params.platform,
      account: params.account,
      cookie_header: params.cookieHeader,
    })
  }
  signOutPlatform = async (platform: string) => {
    await this.session.request(Subjects.platformLoginRemove, { platform })
  }

  listVodsPage = (offset: number, limit: number, rescan = false) =>
    this.session.request<VODListPage>(Subjects.vodsList, { offset, limit, rescan })
  getRecordingThumbnail = async (dir: string, file: string) => {
    const res = await this.session.request<{ jpeg_b64: string }>(
      Subjects.vodsThumbnail,
      { dir, file },
      VODS_THUMBNAIL_TIMEOUT_MS,
    )
    return res.jpeg_b64
  }
  deleteVOD = (dir: string) =>
    this.session.request<string>(Subjects.vodsDelete, { dir })
  deleteVODs = (dirs: string[]) =>
    this.session.request<VODDeleteManyResponse>(
      Subjects.vodsDeleteMany,
      { dirs },
      VODS_DELETE_TIMEOUT_MS,
    )
  cancelMoves = (key?: { platform: string; username: string }) =>
    this.session.request<VODCancelMovesResponse>(
      Subjects.vodsCancelMoves,
      key ?? {},
    )

  // Encode queue (VOD file merging). Queue/progress live state arrives via
  // the snapshot (snapshot.encode), which the session's encode.> watch feeds.
  enqueueMerge = (dir: string, deleteParts: boolean) =>
    this.session.request<EncodeJob>(Subjects.encodeEnqueue, {
      dir,
      delete_parts: deleteParts,
    })
  enqueueConvert = (dir: string, deleteOriginal: boolean) =>
    this.session.request<EncodeJob>(Subjects.encodeEnqueue, {
      dir,
      type: "convert",
      delete_parts: deleteOriginal,
    })
  listEncodeQueue = () =>
    this.session.request<EncodeQueue>(Subjects.encodeList)
  removeEncodeJob = async (id: number) => {
    await this.session.request<string>(Subjects.encodeRemove, { id })
  }
  reorderEncodeJob = async (id: number, direction: "up" | "down" | "top") => {
    await this.session.request<string>(Subjects.encodeReorder, { id, direction })
  }
  cancelEncodeJob = async (id: number) => {
    await this.session.request<string>(Subjects.encodeCancel, { id })
  }
  retryEncodeJob = async (id: number) => {
    await this.session.request<string>(Subjects.encodeRetry, { id })
  }
  setEncodeQueuePaused = async (paused: boolean) => {
    await this.session.request<string>(
      paused ? Subjects.encodePause : Subjects.encodeResume,
    )
  }
  listEncoders = (refresh = false) =>
    this.session.request<EncoderList>(Subjects.encodeEncoders, { refresh })

  // Reconnect only — the Web UI never spawns or downloads a daemon, so the
  // desktop's "downloading"/"no-binary" states can't occur here.
  retryConnect = async (): Promise<ConnectionInfo> => {
    if (
      this.connection.state === "connected" ||
      this.connection.state === "connecting"
    ) {
      return this.connection
    }
    await this.session.reset()
    await this.ensureConnected()
    return this.connection
  }

  addStreamer = ({ monitor, ...ref }: Parameters<DaemonClient["addStreamer"]>[0]) =>
    this.session.request<AddStreamerResult>(
      monitor ? Subjects.streamerAdd : Subjects.streamerRecordOnce,
      refPayload(ref),
      CLOUD_ROUNDTRIP_TIMEOUT_MS,
    )
  removeStreamer = (ref: Parameters<DaemonClient["removeStreamer"]>[0]) =>
    this.mutate(Subjects.streamerRemove, refPayload(ref))
  removeAllStreamers = () => this.mutate(Subjects.streamerRemoveAll, {})
  disableStreamer = (ref: Parameters<DaemonClient["disableStreamer"]>[0]) =>
    this.mutate(Subjects.streamerRecordOff, refPayload(ref))
  disableAllStreamers = () => this.mutate(Subjects.streamerRecordOffAll, {})
  enableStreamer = (ref: Parameters<DaemonClient["enableStreamer"]>[0]) =>
    this.mutate(Subjects.streamerRecordOn, refPayload(ref))
  enableAllStreamers = () => this.mutate(Subjects.streamerRecordOnAll, {})
  checkStreamer = (ref: Parameters<DaemonClient["checkStreamer"]>[0]) =>
    this.session.request<string>(
      Subjects.streamerCheck,
      refPayload(ref),
      CLOUD_ROUNDTRIP_TIMEOUT_MS,
    )
  setStreamerOptions = ({
    platform,
    username,
    url,
    ...opts
  }: Parameters<DaemonClient["setStreamerOptions"]>[0]) =>
    this.mutate(Subjects.streamerSetOptions, {
      ...refPayload({ platform, username, url }),
      ...opts,
    })

  // mutate runs a streamer mutation. The resulting state change arrives via
  // the KV watch, as the daemon rewrites the affected view docs.
  private mutate(subject: string, payload: unknown): Promise<string> {
    return this.session.request<string>(subject, payload)
  }

  // Native helpers: no native dialogs or file manager in a browser. The
  // "external browser" for a stream URL is just another tab.
  chooseDirectory = () => Promise.resolve<string | null>(null)
  openExternal = (url: string) =>
    Promise.resolve(window.open(url, "_blank", "noopener,noreferrer") != null)
  openPath = () => Promise.resolve(false)

  // The daemon has no login items of its own; in a container Docker's
  // restart policy decides.
  getStartAtLogin = () =>
    Promise.resolve<StartAtLogin>({ supported: false, enabled: false })
  setStartAtLogin = () =>
    Promise.resolve<StartAtLogin>({ supported: false, enabled: false })

  mediaUrl = (dir: string, file: string, download = false) =>
    this.urls.media(dir, file, download)

  // Signs this browser out: the daemon revokes the session and closes its
  // sockets, so the page goes to the sign-in page whatever the answer.
  signOut = async () => {
    this.session.beginShutdown()
    try {
      await fetch(this.urls.signout, {
        method: "POST",
        credentials: "same-origin",
      })
    } finally {
      location.replace(this.urls.signin)
    }
  }

  // Quit flow: the Web UI doesn't own the daemon's lifecycle.
  getQuitPrompt = () => Promise.resolve<QuitPrompt | null>(null)
  confirmQuit = () => Promise.resolve(false)
  forceQuit = () => Promise.resolve(false)
  cancelQuit = () => Promise.resolve(false)

  // Updates are read-only here. Nothing is downloaded and nothing can be
  // installed, but the daemon's version and whether it's behind are both in the
  // snapshot already, so the tab can still tell the user where they stand.
  private updateState = (): UpdateState => {
    const { daemon } = this.session.snapshot()
    const currentVersion = daemon.version ?? ""
    const behind = Boolean(
      currentVersion &&
        daemon.recommendedVersion &&
        currentVersion !== daemon.recommendedVersion,
    )
    return {
      app: APP_UPDATE_UNAVAILABLE,
      daemon: {
        phase: "disabled",
        currentVersion,
        availableVersion: behind ? daemon.recommendedVersion : null,
        progress: null,
        message: behind
          ? this.container
            ? "Update the container with `docker compose pull && docker compose up -d`."
            : "Install it from the desktop app, or run `vod update`."
          : null,
        container: this.container,
      },
      install: { phase: "idle", message: null },
    }
  }

  getUpdateState = () => Promise.resolve(this.updateState())
  checkUpdates = () => Promise.resolve(this.updateState())
  installUpdates = () => Promise.resolve(false)

  onStreamersChanged = (cb: (snapshot: StreamersSnapshot) => void) => {
    this.streamerListeners.add(cb)
    return () => {
      this.streamerListeners.delete(cb)
    }
  }
  onConnectionChanged = (cb: (info: ConnectionInfo) => void) => {
    this.connectionListeners.add(cb)
    return () => {
      this.connectionListeners.delete(cb)
    }
  }
  onQuitRequested = () => () => {}
  // No push: the daemon rows are derived from the snapshot, which already
  // streams, so the screen re-renders from the daemon slice instead.
  onUpdateStateChanged = () => () => {}
  onVodsChanged = (cb: () => void) => {
    this.vodsListeners.add(cb)
    return () => {
      this.vodsListeners.delete(cb)
    }
  }
}

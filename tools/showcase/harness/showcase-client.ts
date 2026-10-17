import { wsconnect, type Authenticator } from "@nats-io/nats-core"
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
  PlatformInfo,
  QuitPrompt,
  StartAtLogin,
  StreamersSnapshot,
  UpdateState,
  UserConfig,
} from "@shared/types"
import type { DaemonClient } from "@/lib/daemon/client"

// A few requests make the daemon wait on a remote answer before it can reply,
// so they need a ceiling above the daemon's own.
const CLOUD_ROUNDTRIP_TIMEOUT_MS = 20_000

// ShowcaseDaemonClient is the showcase harness's DaemonClient: the Web UI's
// NATS-over-WebSocket client, dressed as the desktop app. It claims every desktop capability so the UI renders the
// desktop app's affordances, and reports both update halves up to date. Native
// actions (folder pickers, opening files, quitting) do nothing.

export interface ShowcaseDaemonClientOptions {
  // The Demo daemon's NATS WebSocket URL, e.g. "ws://127.0.0.1:14223".
  servers: string | string[]
  // The version the Update and About screens report for the app.
  appVersion: string
}

export class ShowcaseDaemonClient implements DaemonClient {
  readonly capabilities = {
    chooseDirectory: true,
    openPath: true,
    quit: true,
    appUpdate: true,
    installUpdates: true,
    webviewLogin: true,
  }

  private readonly session: DaemonSession
  private readonly servers: string | string[]
  private connection: ConnectionInfo = { state: "connecting" }
  private readonly streamerListeners = new Set<(s: StreamersSnapshot) => void>()
  private readonly connectionListeners = new Set<(c: ConnectionInfo) => void>()
  private readonly vodsListeners = new Set<() => void>()
  private readonly appVersion: string

  constructor(opts: ShowcaseDaemonClientOptions) {
    this.servers = opts.servers
    this.appVersion = opts.appVersion
    this.session = new DaemonSession({
      connect: wsconnect,
      connectOptions: {
        name: "vodlabs-showcase",
      },
      events: {
        onSnapshot: (snap) => {
          for (const cb of this.streamerListeners) cb(snap)
        },
        onConnectionChanged: (info) => this.setConnection(info),
        // The VOD-Archive-changed signal rides the daemon.vods_rev KV key, which
        // the session's existing "daemon.>" watch already mirrors.
        onKvEntryChanged: (key) => {
          if (key === VODS_REV_KEY) {
            for (const cb of this.vodsListeners) cb()
          }
        },
      },
    })
    // A headless page reports itself hidden; the desktop window being shot is not.
    this.session.setVisible(true)

    void this.ensureConnected()
  }

  private setConnection(info: ConnectionInfo): void {
    this.connection = info
    for (const cb of this.connectionListeners) cb(info)
  }

  private async ensureConnected(): Promise<void> {
    this.setConnection({ state: "connecting" })
    try {
      await this.session.connect(this.servers)
      await this.session.start()
    } catch {
      const url = Array.isArray(this.servers)
        ? this.servers.join(", ")
        : this.servers
      this.setConnection({
        state: "disconnected",
        message: `Can't reach the Demo daemon at ${url}. Is it running?`,
      })
    }
  }

  getConnection = () => Promise.resolve(this.connection)
  getSnapshot = () => Promise.resolve(this.session.snapshot())
  getPlatforms = () => this.session.request<PlatformInfo[]>(Subjects.platforms)
  getCloudIntervals = () =>
    this.session.request<CloudIntervals>(Subjects.cloudIntervals)
  getInfo = () => this.session.request<DaemonInfo>(Subjects.info)
  getDesktopLogPath = () =>
    Promise.resolve("/Users/demo/Library/Logs/VodLabs Recorder/desktop.log")

  getConfig = () => this.session.request<UserConfig>(Subjects.configGet)
  setConfig = (cfg: UserConfig) =>
    this.session.request<UserConfig>(Subjects.configSet, cfg)

  // No webview to sign in with: the sign-in window never opens.
  platformLogin = (): Promise<boolean> => Promise.resolve(false)
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

  // Native helpers do nothing: a Shot must never leave the page.
  chooseDirectory = () => Promise.resolve<string | null>(null)
  openExternal = () => Promise.resolve(false)
  openPath = () => Promise.resolve(false)

  getStartAtLogin = () =>
    Promise.resolve<StartAtLogin>({ supported: true, enabled: true })
  setStartAtLogin = (enabled: boolean) =>
    Promise.resolve<StartAtLogin>({ supported: true, enabled })

  getQuitPrompt = () => Promise.resolve<QuitPrompt | null>(null)
  confirmQuit = () => Promise.resolve(false)
  forceQuit = () => Promise.resolve(false)
  cancelQuit = () => Promise.resolve(false)

  // Both halves up to date, as a current desktop install would report.
  private updateState = (): UpdateState => {
    const { daemon } = this.session.snapshot()
    return {
      app: {
        phase: "up-to-date",
        currentVersion: this.appVersion,
        channel: "stable",
        availableVersion: null,
        progress: null,
        message: null,
      },
      daemon: {
        phase: "up-to-date",
        currentVersion: daemon.version ?? "",
        availableVersion: null,
        progress: null,
        message: null,
        container: false,
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
  onUpdateStateChanged = () => () => {}
  onVodsChanged = (cb: () => void) => {
    this.vodsListeners.add(cb)
    return () => {
      this.vodsListeners.delete(cb)
    }
  }
}

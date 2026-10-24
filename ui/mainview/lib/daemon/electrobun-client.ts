import { Electroview } from "electrobun/view"
// The Electrobun RPC schema is desktop-only and lives outside @shared (it pulls
// in electrobun types); this is a type-only import, erased at build time.
import type { AppRPCType } from "../../../../desktop/src/rpc"
import type {
  ConnectionInfo,
  QuitPrompt,
  StreamersSnapshot,
  UpdateState,
} from "@shared/types"
import type { DaemonClient } from "./client"
import { setDaemonClient } from "./registry"

// The webview's single set of push listeners. Populated by the message handlers
// below and fanned out to UI subscribers via on*Changed().
const streamerListeners = new Set<(s: StreamersSnapshot) => void>()
const connectionListeners = new Set<(c: ConnectionInfo) => void>()
const quitListeners = new Set<(p: QuitPrompt) => void>()
const updateListeners = new Set<(s: UpdateState) => void>()
const vodsListeners = new Set<() => void>()

// Single Electroview transport for the whole webview. All bun RPC (native
// helpers + daemon proxy) flows through this; bun pushes arrive as messages.
const rpc = Electroview.defineRPC<AppRPCType>({
  // Electrobun's default is 1000ms, which any interactive request blows past
  // (platformLogin waits for a sign-in, chooseDirectory for a dialog,
  // retryConnect for a daemon spawn) — the webview promise would reject
  // "RPC request timed out." while the bun side kept going. No transport
  // timeout instead: every bun handler is bounded on its own terms (daemon
  // calls sit on a 5s NATS timeout; dialogs/logins legitimately wait for the
  // user).
  maxRequestTime: Infinity,
  handlers: {
    requests: {},
    messages: {
      streamersChanged: (snapshot) => {
        for (const cb of streamerListeners) cb(snapshot)
      },
      connectionChanged: (info) => {
        for (const cb of connectionListeners) cb(info)
      },
      quitRequested: (prompt) => {
        for (const cb of quitListeners) cb(prompt)
      },
      updateStateChanged: (state) => {
        for (const cb of updateListeners) cb(state)
      },
      recordingsChanged: () => {
        for (const cb of vodsListeners) cb()
      },
    },
  },
})

// Construct the Electroview to wire rpc's transport to the bun process.
new Electroview({ rpc })
const req = rpc.request

// Report the webview's visibility to bun so it can pause the high-frequency
// streamersChanged pushes while the window is hidden. The OS suspends the hidden
// WebContent process and buffers those pushes, which otherwise replay in a burst
// (the UI "fast-forwards" through stale stats) when the window regains focus.
function reportVisibility(): void {
  try {
    rpc.send.viewVisibilityChanged({ visible: !document.hidden })
  } catch {
    /* transport not ready; bun assumes visible until told otherwise */
  }
}
document.addEventListener("visibilitychange", reportVisibility)
reportVisibility()

const electrobunClient: DaemonClient = {
  // The desktop host supports everything: native dialogs, file manager,
  // daemon lifecycle (quit flow), and Electrobun self-update.
  capabilities: {
    chooseDirectory: true,
    openPath: true,
    quit: true,
    appUpdate: true,
    installUpdates: true,
    webviewLogin: true,
    // The desktop app reaches its own daemon with no sign-in.
    webSession: false,
    // Recordings are on this machine; the OS player opens them.
    browserMedia: false,
  },

  getConnection: () => req.getConnection({}),
  getSnapshot: () => req.getSnapshot({}),
  getPlatforms: () => req.getPlatforms({}),
  getCloudIntervals: () => req.getCloudIntervals({}),
  getInfo: () => req.getInfo({}),
  getDesktopLogPath: () => req.getDesktopLogPath({}),
  getUpdateState: () => req.getUpdateState({}),
  checkUpdates: () => req.checkUpdates({}),
  installUpdates: () => req.installUpdates({}),
  getConfig: () => req.getConfig({}),
  setConfig: (cfg) => req.setConfig(cfg),
  retryConnect: () => req.retryConnect({}),

  getStartAtLogin: () => req.getStartAtLogin({}),
  setStartAtLogin: (enabled) => req.setStartAtLogin({ enabled }),
  signOut: () => Promise.resolve(),
  mediaUrl: () => "",

  // Platform logins run in the bun process (only it can open a webview and read
  // its cookie store). PRIVACY: cookies are captured in bun and handed to the
  // local daemon; they never reach the vodlabs cloud.
  platformLogin: (platform) => req.platformLogin({ platform }),
  setPlatformCookies: async (params) => {
    await req.setPlatformCookies(params)
  },
  signOutPlatform: async (platform) => {
    await req.signOutPlatform({ platform })
  },

  listVodsPage: (offset, limit, rescan = false) =>
    req.listVodsPage({ offset, limit, rescan }),
  getRecordingThumbnail: (dir, file) => req.getRecordingThumbnail({ dir, file }),
  deleteVOD: (dir) => req.deleteVOD({ dir }),
  deleteVODs: (dirs) => req.deleteVODs({ dirs }),
  cancelMoves: (key) => req.cancelMoves(key ?? {}),

  enqueueMerge: (dir, deleteParts) => req.enqueueMerge({ dir, deleteParts }),
  enqueueConvert: (dir, deleteOriginal) =>
    req.enqueueConvert({ dir, deleteOriginal }),
  listEncodeQueue: () => req.listEncodeQueue({}),
  removeEncodeJob: async (id) => {
    await req.removeEncodeJob({ id })
  },
  reorderEncodeJob: async (id, direction) => {
    await req.reorderEncodeJob({ id, direction })
  },
  cancelEncodeJob: async (id) => {
    await req.cancelEncodeJob({ id })
  },
  retryEncodeJob: async (id) => {
    await req.retryEncodeJob({ id })
  },
  setEncodeQueuePaused: async (paused) => {
    await req.setEncodeQueuePaused({ paused })
  },
  listEncoders: (refresh = false) => req.listEncoders({ refresh }),

  getQuitPrompt: () => req.getQuitPrompt({}),
  confirmQuit: () => req.confirmQuit({}),
  forceQuit: () => req.forceQuit({}),
  cancelQuit: () => req.cancelQuit({}),

  addStreamer: (params) => req.addStreamer(params),
  removeStreamer: (ref) => req.removeStreamer(ref),
  removeAllStreamers: () => req.removeAllStreamers({}),
  disableStreamer: (ref) => req.disableStreamer(ref),
  disableAllStreamers: () => req.disableAllStreamers({}),
  enableStreamer: (ref) => req.enableStreamer(ref),
  enableAllStreamers: () => req.enableAllStreamers({}),
  checkStreamer: (ref) => req.checkStreamer(ref),
  setStreamerOptions: (params) => req.setStreamerOptions(params),

  chooseDirectory: (startingFolder) => req.chooseDirectory({ startingFolder }),
  openExternal: (url) => req.openExternal({ url }),
  openPath: (path) => req.openPath({ path }),

  onStreamersChanged(cb) {
    streamerListeners.add(cb)
    return () => {
      streamerListeners.delete(cb)
    }
  },
  onConnectionChanged(cb) {
    connectionListeners.add(cb)
    return () => {
      connectionListeners.delete(cb)
    }
  },
  onQuitRequested(cb) {
    quitListeners.add(cb)
    return () => {
      quitListeners.delete(cb)
    }
  },
  onUpdateStateChanged(cb) {
    updateListeners.add(cb)
    return () => {
      updateListeners.delete(cb)
    }
  },
  onVodsChanged(cb) {
    vodsListeners.add(cb)
    return () => {
      vodsListeners.delete(cb)
    }
  },
}

// Install as the process-wide client (side effect on import). The desktop
// entry (main.tsx) imports this module before the App module so components
// that resolve the client at module scope find it installed.
setDaemonClient(electrobunClient)

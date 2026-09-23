import Electrobun, {
  BrowserWindow,
  BrowserView,
  Tray,
  Updater,
  Utils,
  ApplicationMenu,
  app,
} from "electrobun/bun";
import type { AppRPCType } from "../rpc";
import type {
  VODDeleteManyResponse,
  VODCancelMovesResponse,
  AddStreamerResult,
  CloudIntervals,
  ConnectionInfo,
  DaemonInfo,
  PlatformInfo,
  QuitPrompt,
  SetPlatformLoginParams,
  SetStreamerOptionsParams,
  StreamerRef,
  UpdateState,
  UserConfig,
} from "../../../shared/types";
import {
  START_HIDDEN_FLAG,
  getStartAtLogin,
  setStartAtLogin,
  startAtLoginSupported,
} from "./start-at-login";
import { connect } from "@nats-io/transport-node";
import { DaemonSession } from "../../../shared/daemon/session";
import {
  PLATFORM_LOGIN_REFRESH_SUBJECT,
  Subjects,
  VODS_DELETE_TIMEOUT_MS,
  VODS_REV_KEY,
  VODS_THUMBNAIL_TIMEOUT_MS,
  refPayload,
} from "../../../shared/daemon/subjects";
import { countPendingWork, hasPendingWork } from "../../../shared/daemon/pending-work";
import { AppUpdater } from "./app-update";
import { DaemonUpdater, type DaemonHost } from "./daemon-update";
import { applyWindowIcon } from "./win-window-icon";
import { acquireSingleInstanceLock } from "./single-instance";
import {
  refreshPlatformLogin,
  runPlatformLogin,
  signOutPlatform,
} from "./platform-login";
import { subscribeBrowserFetch } from "./browser-context";
import {
  DEFAULT_BIN_REPO,
  downloadDaemon,
  latestDaemonVersion,
  locateDaemonBinary,
  spawnDaemon,
} from "./daemon-process";

import { desktopLogPath, installFileLogging } from "./log-file";

// Before anything else logs: everything above this point lands only on stdout.
installFileLogging();

// IPv4 explicitly, matching Vite's server.host: "localhost" can resolve to a
// different server holding the same port on ::1.
const DEV_SERVER_PORT = 5173;
const DEV_SERVER_URL = `http://127.0.0.1:${DEV_SERVER_PORT}`;
const NATS_URL = process.env.VLREC_NATS_URL ?? "nats://127.0.0.1:4222";
// A few requests make the daemon wait on a remote answer before it can reply,
// so they need a ceiling above the daemon's own. Time out below it and the user
// gets a generic local timeout instead of the daemon's own account of what
// went wrong.
const CLOUD_ROUNDTRIP_TIMEOUT_MS = 20_000;

// session owns the daemon connection: NATS transport, KV mirroring into the
// snapshot, reconnect resync, and debounced snapshot delivery (see
// shared/daemon/session.ts — the same engine drives the Web UI).
// The events bridge it to the webview RPC and the desktop-only concerns.
const session = new DaemonSession({
  connect,
  events: {
    // Push the snapshot to the webview. Report delivery so a push dropped
    // before the RPC transport is wired is re-sent on the next change (the
    // webview also pulls the snapshot on mount).
    onSnapshot: (snap) => {
      try {
        rpc.send.streamersChanged(snap);
        return true;
      } catch {
        return false; // transport not ready
      }
    },
    onConnectionChanged: (info) => setConnection(info),
    // Daemon update availability rides on these two keys; re-evaluate (and, when
    // a new version is due, auto-download it) whenever either changes.
    // Independent of webview visibility — the download and the native
    // notification should happen even when closed to the tray.
    onKvEntryChanged: (key) => {
      if (key === "daemon.version" || key === "daemon.upstream_version") {
        void daemonUpdater.check();
      }
      if (key === VODS_REV_KEY) {
        pushVodsChanged();
      }
    },
    // During a graceful quit the daemon keeps its NATS server up until it has
    // fully shut down (recordings stopped, processing drained), so the first
    // disconnect after we signalled shutdown means it's gone — exit the app.
    // The same signal drives the update restart, which waits for the old daemon
    // to exit before launching the new binary.
    onDisconnect: () => {
      if (quitting) {
        Utils.quit();
        return;
      }
      const resolve = daemonExitResolve;
      daemonExitResolve = null;
      resolve?.();
    },
  },
});
// Desktop app self-update (Electrobun Updater). Independent of the daemon
// connection: it checks GitHub releases, auto-downloads, and notifies. See
// app-update.ts.
const appUpdater = new AppUpdater();
// Daemon update: watches the snapshot for a newer recommended_version,
// auto-downloads that binary, and can restart the daemon onto it. See
// daemon-update.ts. Both updaters push through one combined UpdateState.
const daemonUpdater = new DaemonUpdater(daemonHost());

let connection: ConnectionInfo = { state: "connecting" };
// Whether the connected daemon runs in a container (Docker owns its lifecycle).
// Set after connecting via the info endpoint; gates quit behaviour — we never
// shut down a containerized daemon.
let daemonContainer = false;
// The binary the running daemon was launched from, also read from the info
// endpoint. Kept so an update restart can put the previous version back if the
// new one doesn't come up.
let daemonExePath: string | null = null;
// Whether the info endpoint above has answered for the current connection. The
// KV snapshot arrives first, so the update check waits on this rather than
// judging a containerized daemon by a not-yet-known flag.
let daemonInfoReady = false;
// Quit bookkeeping. `quitting` latches once we've asked the daemon to shut down,
// so the session's onDisconnect treats the next disconnect as "daemon gone →
// exit".
// `quitPrompt` holds the in-flight work summary for the quit dialog (pushed, and
// pullable on a fresh window). `quitTimer` is the force-quit fail-safe exit (only
// armed on an explicit "Quit now").
let quitting = false;
let quitPrompt: QuitPrompt | null = null;
let quitTimer: ReturnType<typeof setTimeout> | null = null;
// One-shot resolver armed by waitForDaemonExit and fired by the session's
// onDisconnect. Distinct from `quitting`: an update restart wants to observe the
// daemon's exit WITHOUT exiting the app along with it.
let daemonExitResolve: (() => void) | null = null;
// The combined install sequence behind the Update tab's single button.
let installState: UpdateState["install"] = { phase: "idle", message: null };
// The app's release channel, resolved once at startup (before anything can act
// on it). "dev" means an unpackaged build run from the repo, where neither half
// self-updates — see the guard in DaemonUpdater.
let appChannel = "";

// MARK: RPC
// ---------------------------------------------------------------------------
// RPC handlers (webview → bun). The webview also pulls initial state via
// getConnection/getSnapshot on mount, so this is the source of truth; bun pushes
// (rpc.send.*) keep it live afterward.
// ---------------------------------------------------------------------------

const rpc = BrowserView.defineRPC<AppRPCType>({
  handlers: {
    requests: {
      chooseDirectory: async ({ startingFolder }) => {
        const chosen = await Utils.openFileDialog({
          startingFolder: startingFolder || Utils.paths.home,
          allowedFileTypes: "*",
          canChooseFiles: false,
          canChooseDirectory: true,
          allowsMultipleSelection: false,
        });
        return chosen && chosen.length > 0 ? chosen[0] : null;
      },
      openExternal: async ({ url }) => Utils.openExternal(url),
      openPath: async ({ path }) => Utils.openPath(path),

      getConnection: () => connection,
      getSnapshot: () => session.snapshot(),
      getPlatforms: () => session.request<PlatformInfo[]>(Subjects.platforms),
      getCloudIntervals: () =>
        session.request<CloudIntervals>(Subjects.cloudIntervals),
      getInfo: () => session.request<DaemonInfo>(Subjects.info),
      getDesktopLogPath: () => desktopLogPath(),

      // Updates. getUpdateState pulls current state on mount; checkUpdates
      // re-checks both halves (each auto-downloads what it finds, progress via
      // the updateStateChanged push); installUpdates restarts the daemon onto
      // its new binary and then installs the app build and relaunches.
      getUpdateState: () => updateState(),
      checkUpdates: async () => {
        await Promise.allSettled([appUpdater.check(), daemonUpdater.check()]);
        return updateState();
      },
      installUpdates: () => installUpdates(),
      getConfig: () => session.request<UserConfig>(Subjects.configGet),
      setConfig: (cfg: UserConfig) =>
        session.request<UserConfig>(Subjects.configSet, cfg),
      getStartAtLogin: async () => ({
        supported: startAtLoginSupported(),
        enabled: await getStartAtLogin(),
      }),
      setStartAtLogin: async ({ enabled }) => ({
        supported: startAtLoginSupported(),
        enabled: await setStartAtLogin(enabled),
      }),
      retryConnect: () => retryConnect(),

      // vodlabs.io account. Sign-in opens the system browser (via
      // openExternal from the webview); the daemon polls for confirmation and

      // Third-party platform logins (Accounts page). PRIVACY: cookies are
      // captured in this bun process and stored on the LOCAL daemon; they are
      // never sent to the vodlabs cloud. platformLogin opens the webview flow;
      // setPlatformCookies is the advanced paste path; signOutPlatform removes
      // the login and clears the login webview's own data.
      platformLogin: ({ platform }) => runPlatformLogin(session, platform),
      setPlatformCookies: async ({
        platform,
        account,
        cookieHeader,
      }: SetPlatformLoginParams) => {
        await session.request(Subjects.platformLoginSet, {
          platform,
          account,
          cookie_header: cookieHeader,
        });
        return true;
      },
      signOutPlatform: async ({ platform }) => {
        await signOutPlatform(session, platform);
        return true;
      },

      // Quit flow. getQuitPrompt lets a freshly-opened window pull a prompt it
      // may have missed (the push can fire before the webview mounts).
      getQuitPrompt: () => quitPrompt,
      // "Finish work, then quit": wait indefinitely so post-processing isn't
      // cut short (null fail-safe). "Quit now": bounded, like an idle quit.
      confirmQuit: () => {
        signalDaemonShutdown(false, null);
        return true;
      },
      forceQuit: () => {
        signalDaemonShutdown(true, QUIT_FAILSAFE_MS);
        return true;
      },
      cancelQuit: () => {
        quitPrompt = null;
        return true;
      },

      // Not mutate(): the add replies with the resolved identity rather than
      // an acknowledgement string.
      addStreamer: ({ monitor, ...ref }) =>
        session.request<AddStreamerResult>(
          monitor ? Subjects.streamerAdd : Subjects.streamerRecordOnce,
          refPayload(ref),
          CLOUD_ROUNDTRIP_TIMEOUT_MS,
        ),
      removeStreamer: (ref: StreamerRef) =>
        mutate(Subjects.streamerRemove, refPayload(ref)),
      removeAllStreamers: () => mutate(Subjects.streamerRemoveAll, {}),
      disableStreamer: (ref: StreamerRef) =>
        mutate(Subjects.streamerRecordOff, refPayload(ref)),
      disableAllStreamers: () => mutate(Subjects.streamerRecordOffAll, {}),
      enableStreamer: (ref: StreamerRef) =>
        mutate(Subjects.streamerRecordOn, refPayload(ref)),
      enableAllStreamers: () => mutate(Subjects.streamerRecordOnAll, {}),
      checkStreamer: (ref: StreamerRef) =>
        session.request<string>(
          Subjects.streamerCheck,
          refPayload(ref),
          CLOUD_ROUNDTRIP_TIMEOUT_MS,
        ),
      setStreamerOptions: ({
        platform,
        username,
        url,
        ...opts
      }: SetStreamerOptionsParams) =>
        mutate(Subjects.streamerSetOptions, {
          ...refPayload({ platform, username, url }),
          ...opts,
        }),

      // VOD Archive: plain daemon proxies. The paging loop lives in the
      // webview (fetchRecordings), which renders pages as they arrive.
      listVodsPage: ({ offset, limit, rescan }) =>
        session.request(Subjects.vodsList, { offset, limit, rescan }),
      getRecordingThumbnail: async ({ dir, file }) => {
        const res = await session.request<{ jpeg_b64: string }>(
          Subjects.vodsThumbnail,
          { dir, file },
          VODS_THUMBNAIL_TIMEOUT_MS,
        );
        return res.jpeg_b64;
      },
      deleteVOD: ({ dir }) =>
        session.request<string>(Subjects.vodsDelete, { dir }),
      deleteVODs: ({ dirs }) =>
        session.request<VODDeleteManyResponse>(
          Subjects.vodsDeleteMany,
          { dirs },
          VODS_DELETE_TIMEOUT_MS,
        ),
      cancelMoves: ({ platform, username }) =>
        session.request<VODCancelMovesResponse>(
          Subjects.vodsCancelMoves,
          platform && username ? { platform, username } : {},
        ),

      // Encode queue (VOD file merging): plain daemon proxies. Live queue
      // rev/progress ride the streamersChanged snapshot (snapshot.encode).
      enqueueMerge: ({ dir, deleteParts }) =>
        session.request(Subjects.encodeEnqueue, {
          dir,
          delete_parts: deleteParts,
        }),
      listEncodeQueue: () => session.request(Subjects.encodeList),
      removeEncodeJob: async ({ id }) => {
        await session.request(Subjects.encodeRemove, { id });
        return true;
      },
      reorderEncodeJob: async ({ id, direction }) => {
        await session.request(Subjects.encodeReorder, { id, direction });
        return true;
      },
      cancelEncodeJob: async ({ id }) => {
        await session.request(Subjects.encodeCancel, { id });
        return true;
      },
      retryEncodeJob: async ({ id }) => {
        await session.request(Subjects.encodeRetry, { id });
        return true;
      },
      setEncodeQueuePaused: async ({ paused }) => {
        await session.request(
          paused ? Subjects.encodePause : Subjects.encodeResume,
        );
        return true;
      },
      listEncoders: ({ refresh }) =>
        session.request(Subjects.encodeEncoders, { refresh: !!refresh }),
    },
    messages: {
      // The webview reports its Page Visibility so the session can pause
      // high-frequency snapshot pushes while it's hidden.
      viewVisibilityChanged: ({ visible }) => session.setVisible(visible),
    },
  },
});

// MARK: Push
// ---------------------------------------------------------------------------
// Push helpers (bun → webview). Sends are best-effort before the webview RPC
// transport is wired (early sends are dropped; the webview pulls on mount).
// ---------------------------------------------------------------------------

function pushConnection(): void {
  try {
    rpc.send.connectionChanged(connection);
  } catch {
    /* transport not ready */
  }
}

// updateState folds both updaters and the install sequence into the single
// object the Update tab renders, so its three parts can never disagree.
function updateState(): UpdateState {
  return {
    app: appUpdater.getStatus(),
    daemon: daemonUpdater.getStatus(),
    install: installState,
  };
}

// pushUpdateState forwards any update transition to the webview. Best-effort:
// the freshly-mounted webview pulls via getUpdateState, so an early drop before
// the transport is wired is harmless.
function pushUpdateState(): void {
  try {
    rpc.send.updateStateChanged(updateState());
  } catch {
    /* transport not ready; the webview pulls state on mount */
  }
}
appUpdater.setOnChange(pushUpdateState);
daemonUpdater.setOnChange(pushUpdateState);

function setInstall(phase: UpdateState["install"]["phase"], message: string | null): void {
  installState = { phase, message };
  pushUpdateState();
}

function setConnection(info: ConnectionInfo): void {
  connection = info;
  pushConnection();
}

// pushVodsChanged tells the webview the VOD Archive listing changed. Best-effort:
// a not-yet-mounted VOD Archive screen pulls a fresh listing on mount anyway.
function pushVodsChanged(): void {
  try {
    rpc.send.recordingsChanged({});
  } catch {
    /* transport not ready */
  }
}

// pushQuitRequested asks the webview to show the quit dialog. Best-effort: if the
// transport isn't ready (window just reopened from the tray), the freshly-mounted
// webview pulls the same prompt via getQuitPrompt.
function pushQuitRequested(): void {
  if (!quitPrompt) return;
  try {
    rpc.send.quitRequested(quitPrompt);
  } catch {
    /* transport not ready; the webview pulls on mount */
  }
}

// How long to wait for applyUpdate() to actually replace the app and exit the
// process before concluding it silently did nothing (Electrobun's applyUpdate is
// a no-op when no build was downloaded).
const APPLY_FAILSAFE_MS = 10_000;
// How long a daemon gets to register its vlapi endpoints once its NATS server
// is accepting connections, and how long a freshly spawned one gets in total.
const SERVICE_READY_MS = 15_000;
const DAEMON_SPAWN_TIMEOUT_MS = 30_000;

// MARK: Updates
// ---------------------------------------------------------------------------
// The Update tab's single "Install and restart" button. The daemon goes first:
// it's restarted in place and verified healthy before the app installs its own
// build and relaunches. If the daemon doesn't come back the app update is
// abandoned, so a bad release can't leave the user with a new app and no
// working recorder.
// ---------------------------------------------------------------------------

// waitForDaemonExit resolves true once the daemon's NATS connection drops (the
// session's onDisconnect), false if it hasn't within timeoutMs. Mirrors the CLI's
// wait in cmd_quit_restart.go.
function waitForDaemonExit(timeoutMs: number): Promise<boolean> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      daemonExitResolve = null;
      resolve(false);
    }, timeoutMs);
    daemonExitResolve = () => {
      clearTimeout(timer);
      resolve(true);
    };
  });
}

// waitForService polls the info endpoint until the daemon actually answers,
// returning what it said (or null on timeout).
//
// Readiness is deliberately not "the connection succeeded": the daemon's NATS
// server binds its port before the vlapi service registers its endpoints, so a
// connect can succeed against a daemon that can't answer anything yet. Go's
// vlupdate.WaitReady draws the same distinction for the CLI.
//
// Returning the info rather than re-fetching it is what makes onConnected's
// state unconditional: there is no second request left to fail.
async function waitForService(timeoutMs: number): Promise<DaemonInfo | null> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      return await session.request<DaemonInfo>(Subjects.info);
    } catch {
      if (Date.now() >= deadline) return null;
      await Bun.sleep(500); // service not registered yet
    }
  }
}

// waitForDaemonReady polls until a freshly-spawned daemon is serving, then
// starts the session (KV watches, snapshot).
async function waitForDaemonReady(timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  let connected = false;
  while (Date.now() < deadline && !connected) {
    await Bun.sleep(500);
    connected = await connectOnce();
  }
  if (!connected) return false;

  const info = await waitForService(Math.max(0, deadline - Date.now()));
  if (!info) return false;
  await onConnected(info);
  return true;
}

// daemonHost is the lifecycle surface DaemonUpdater drives. Injected rather than
// imported so daemon-update.ts doesn't have to depend on this module.
function daemonHost(): DaemonHost {
  return {
    version: () => session.snapshot().daemon.version,
    recommendedVersion: () => session.snapshot().daemon.recommendedVersion,
    // A daemon the service has stopped serving cannot be told which version to
    // move to by anything but the heartbeat, and a heartbeat that never lands
    // leaves it stuck. The release itself is the other answer, so the updater
    // falls back to it — see AppUpdater/DaemonUpdater.updateRequired.
    updateRequired: () => session.snapshot().daemon.updateRequired,
    container: () => daemonContainer,
    // VLREC_DEV_UPDATES re-enables the daemon update path in a dev build, which
    // is otherwise the one configuration where this flow can't be exercised at
    // all. Pair it with the GIT_TAG override in scripts/build.sh to stamp a
    // deliberately old daemon and watch the real download → restart run.
    devBuild: () => appChannel === "dev" && !process.env.VLREC_DEV_UPDATES,
    infoReady: () => daemonInfoReady,
    executablePath: () => daemonExePath,
    shutdown: async () => {
      // Non-force: recording stops at once, post-processing drains. Latch
      // shutdown mode first so an instant disconnect can't race into the
      // reconnect banner — reset() clears it again.
      session.beginShutdown();
      setConnection({ state: "connecting", message: "Updating the recorder…" });
      await session
        .request<string>(Subjects.shutdown)
        .catch(() => {
          /* daemon may already be exiting; we still wait for the disconnect */
        });
    },
    waitForExit: waitForDaemonExit,
    resetSession: () => session.reset(),
    spawn: (exePath) => {
      spawnDaemon(exePath);
      setConnection({ state: "connecting", message: "Starting the recorder…" });
    },
    waitForReady: waitForDaemonReady,
  };
}

// installUpdates runs the combined sequence behind the single button. Returns
// false if it stopped early; the tab reads the reason from install.message.
async function installUpdates(): Promise<boolean> {
  if (installState.phase !== "idle" && installState.phase !== "error") {
    return false; // already running
  }

  if (daemonUpdater.isReady()) {
    setInstall("restarting-daemon", null);
    const res = await daemonUpdater.restart();
    if (!res.ok) {
      // The daemon didn't come up on the new version (or is still draining).
      // Leave the app where it is — one broken component beats two.
      setInstall("error", res.message);
      return false;
    }
  }

  if (appUpdater.isReady()) {
    setInstall("installing-app", null);
    // Electrobun's applyUpdate() ends by calling quit(), which fires before-quit
    // — and our handler vetoes that and reroutes into requestQuit(). Latch
    // `quitting` first so it passes straight through. Setting it directly rather
    // than going via requestQuit() is also what stops the app from shutting the
    // freshly-restarted daemon back down: it's unref'd, so it survives the
    // relaunch and the new app just reconnects to it.
    quitting = true;
    try {
      await appUpdater.apply();
    } catch (err) {
      quitting = false;
      setInstall("error", err instanceof Error ? err.message : String(err));
      return false;
    }
    // applyUpdate() normally exits the process mid-call. If we're still here the
    // install didn't take, so un-latch rather than leave the app unable to quit.
    setTimeout(() => {
      if (installState.phase === "installing-app") {
        quitting = false;
        setInstall("error", "The update didn't install. Try again, or reinstall the app.");
      }
    }, APPLY_FAILSAFE_MS);
    return true;
  }

  setInstall("idle", null);
  return true;
}

// MARK: Daemon connection

async function connectOnce(): Promise<boolean> {
  try {
    await session.connect(NATS_URL);
    return true;
  } catch {
    return false;
  }
}

// devDaemonRunExternally reports whether this run leaves the daemon's lifecycle
// to the developer. VLREC_DEV_SPAWN_DAEMON=1 enables the spawn/download path in
// a dev build, which is otherwise the one configuration where it can't be
// exercised at all.
function devDaemonRunExternally(): boolean {
  return appChannel === "dev" && !process.env.VLREC_DEV_SPAWN_DAEMON;
}

// EXTERNAL_DAEMON_POLL_MS paces the wait for a daemon someone else starts. The
// wait has no deadline: the developer may start it minutes after the app.
const EXTERNAL_DAEMON_POLL_MS = 1000;

// waitForExternalDaemon polls until a daemon started outside the app is serving,
// holding the banner in "connecting" (spinner, no Retry) for as long as it takes.
async function waitForExternalDaemon(): Promise<void> {
  setConnection({
    state: "connecting",
    message: "Waiting for a daemon...",
  });
  console.log(
    "Dev build: not spawning a daemon. Waiting for one on " +
      `${NATS_URL} (set VLREC_DEV_SPAWN_DAEMON=1 to spawn one).`,
  );
  for (;;) {
    if (quitting) return;
    if (await connectOnce()) {
      const info = await waitForService(SERVICE_READY_MS);
      if (info) {
        await onConnected(info);
        return;
      }
      // The port is held by something that isn't answering — a daemon still
      // starting up, or a stale process. Drop the connection so the next
      // attempt dials fresh.
      await session.reset();
    }
    await Bun.sleep(EXTERNAL_DAEMON_POLL_MS);
  }
}

async function ensureConnected(): Promise<void> {
  setConnection({ state: "connecting" });

  if (devDaemonRunExternally()) {
    await waitForExternalDaemon();
    return;
  }

  if (await connectOnce()) {
    // Something already holds the port. If it's a daemon still registering its
    // endpoints, wait for it — spawning a second one would only fail to bind.
    const info = await waitForService(SERVICE_READY_MS);
    if (info) {
      await onConnected(info);
      return;
    }
    setConnection({
      state: "disconnected",
      message: "The recorder is running but not responding.",
    });
    return;
  }

  // No daemon is running. Find a binary to spawn — already installed (env
  // override, on PATH, or previously downloaded), otherwise download the latest
  // release from GitHub. A non-technical user never has to know any of this
  // happened, nor set VLREC_DAEMON_PATH.
  let exe = await locateDaemonBinary();
  if (!exe) {
    exe = await tryDownloadDaemon();
  }
  if (!exe) {
    setConnection({
      state: "no-binary",
      message:
        "Couldn't download the recorder. Check your internet connection and try again.",
    });
    return;
  }

  spawnDaemon(exe);
  setConnection({ state: "connecting", message: "Starting the recorder…" });
  if (await waitForDaemonReady(DAEMON_SPAWN_TIMEOUT_MS)) return;
  setConnection({
    state: "disconnected",
    message: "The recorder was started but did not become ready in time.",
  });
}

// onConnected starts the session (connected state, KV watches, status loop) and
// records the daemon's container flag and binary path, which gate quit behaviour
// and the update rollback. The info comes from the readiness probe that just
// proved the daemon is serving, so these are always set on a live connection.
async function onConnected(info: DaemonInfo): Promise<void> {
  daemonContainer = info.container;
  daemonExePath = info.executable_path || null;
  daemonInfoReady = true;
  await session.start();
  void daemonUpdater.check();
  startLoginRefresh();
  startBrowserFetch();
}

// startBrowserFetch (re)wires the LOCAL request-reply handler the daemon uses to
// run signed platform requests in a login webview. Re-created on every connect,
// like startLoginRefresh, since subscriptions die with the previous connection.
let unsubscribeBrowserFetch: (() => void) | null = null;
function startBrowserFetch(): void {
  try {
    unsubscribeBrowserFetch?.();
  } catch {
    /* previous connection already closed */
  }
  unsubscribeBrowserFetch = subscribeBrowserFetch(session);
}

// MARK: Platform login refresh
// ---------------------------------------------------------------------------
// Captured platform-login jars go stale within tens of minutes (short-lived
// tokens like TikTok's msToken stop rotating once captured), which silently
// stops the daemon's authed re-checks from unlocking restricted rooms. While
// the desktop app runs it freshens every stored login in a hidden webview: on
// connect, every LOGIN_REFRESH_INTERVAL_MS, and on demand when the daemon
// publishes PLATFORM_LOGIN_REFRESH_SUBJECT (an authed check that stayed restricted).
// PRIVACY: entirely local — the hidden webview talks to the platform's own
// site and the captured jar goes to the local daemon; nothing touches the
// vodlabs cloud.
// ---------------------------------------------------------------------------

const LOGIN_REFRESH_INTERVAL_MS = 10 * 60 * 1000;
let loginRefreshTimer: ReturnType<typeof setInterval> | null = null;
let unsubscribeLoginRefresh: (() => void) | null = null;

// startLoginRefresh (re)wires the on-demand subscription and starts the
// periodic sweep. Called from onConnected on EVERY successful connect:
// retryConnect() resets the NATS connection, which drops subscriptions made on
// the previous one, so the subscription must be re-created here each time.
function startLoginRefresh(): void {
  try {
    unsubscribeLoginRefresh?.();
  } catch {
    /* previous connection already closed */
  }
  unsubscribeLoginRefresh = session.subscribeJSON<{ platform: string }>(
    PLATFORM_LOGIN_REFRESH_SUBJECT,
    ({ platform }) => {
      if (platform) void refreshOneLogin(platform);
    },
  );
  if (!loginRefreshTimer) {
    loginRefreshTimer = setInterval(
      () => void refreshAllLogins(),
      LOGIN_REFRESH_INTERVAL_MS,
    );
  }
  // Refresh immediately: whatever jar the daemon holds predates this app run.
  void refreshAllLogins();
}

async function refreshOneLogin(platform: string): Promise<void> {
  try {
    const result = await refreshPlatformLogin(session, platform);
    if (result !== "skipped") {
      console.log(`platform login refresh (${platform}): ${result}`);
    }
  } catch (err) {
    console.error(`platform login refresh (${platform}) failed:`, err);
  }
}

async function refreshAllLogins(): Promise<void> {
  if (connection.state !== "connected") return;
  try {
    const logins = await session.request<{ platform: string }[] | null>(
      Subjects.platformLoginList,
    );
    // Sequential on purpose: at most one hidden webview at a time.
    for (const l of logins ?? []) {
      await refreshOneLogin(l.platform);
    }
  } catch (err) {
    console.error("platform login refresh sweep failed:", err);
  }
}

// tryDownloadDaemon fetches the latest daemon release, surfacing progress through
// the connection banner. Returns the installed path, or null on failure (e.g. the
// user is offline and nothing is cached).
async function tryDownloadDaemon(): Promise<string | null> {
  setConnection({
    state: "downloading",
    message: "Setting up the recorder daemon… (one-time setup)",
  });
  try {
    const repo = process.env.VLREC_BIN_REPO ?? DEFAULT_BIN_REPO;
    const version = await latestDaemonVersion(repo);
    return await downloadDaemon(repo, version, (pct) =>
      setConnection({
        state: "downloading",
        message: `Downloading the recorder daemon… ${pct}%`,
      }),
    );
  } catch (err) {
    console.error("Daemon download failed:", err);
    return null;
  }
}

async function retryConnect(): Promise<ConnectionInfo> {
  if (
    connection.state === "connected" ||
    connection.state === "connecting" ||
    connection.state === "downloading"
  ) {
    return connection;
  }
  await session.reset();
  await ensureConnected();
  return connection;
}

// mutate runs a streamer mutation. The resulting state change arrives via the
// KV watch, as the daemon rewrites the affected view docs.
function mutate(
  subject: string,
  payload: unknown,
  timeoutMs?: number,
): Promise<string> {
  return session.request<string>(subject, payload, timeoutMs);
}

// MARK: Quit
// ---------------------------------------------------------------------------
// Quitting (tray/menu) gracefully stops a host daemon so background recording
// and post-processing wrap up before the app exits. A containerized daemon is
// left to Docker. When work is in flight we surface it and let the user wait or
// force-exit. (Closing the window via the title-bar X is NOT a quit — that just
// hides to the tray; see createMainWindow.)
// ---------------------------------------------------------------------------

// Fail-safe window for a shutdown we don't expect to wait on (idle quit, or the
// user's "Quit now"): if the daemon's disconnect isn't observed by then, exit
// anyway so the app can't hang. Generous enough for an idle daemon to stop on
// its own first; the wait-for-processing path passes null instead.
const QUIT_FAILSAFE_MS = 5000;

// pendingWorkCounts summarizes in-flight work from the latest snapshot so the
// quit dialog can tell the user what quitting will interrupt or finish. The
// Update tab warns off the same counts before an install restarts the daemon.
function pendingWorkCounts(): QuitPrompt {
  return countPendingWork(session.snapshot().streamers);
}

// requestQuit is the single entry point for both quit triggers (tray + app
// menu). It chooses between an immediate exit, a silent graceful shutdown, and
// prompting the user when work is in flight.
function requestQuit(): void {
  if (quitting) return;

  // Not connected, or the daemon is Docker-managed: just close the app and let
  // the daemon's owner (Docker, or whoever started it) manage its lifecycle.
  if (connection.state !== "connected" || daemonContainer) {
    quitting = true;
    Utils.quit();
    return;
  }

  const pending = pendingWorkCounts();
  if (!hasPendingWork(pending)) {
    // Nothing to lose — shut down and exit. Bound the wait: there's no
    // processing to protect, so the fail-safe only guarantees we actually quit
    // if the daemon's disconnect isn't observed (see signalDaemonShutdown).
    signalDaemonShutdown(false, QUIT_FAILSAFE_MS);
    return;
  }

  // Work in flight: surface it and wait for the user's choice. Don't signal
  // shutdown yet — cancelling must leave recordings untouched. Reopen the window
  // if it was closed to the tray; the freshly-mounted webview pulls the prompt.
  quitPrompt = pending;
  showMainWindow();
  pushQuitRequested();
}

// signalDaemonShutdown asks the daemon to stop (force = skip pending
// processing) and exits once its NATS connection drops (the session's
// onDisconnect event).
//
// failsafeMs arms a timer that exits anyway if that disconnect never arrives,
// so the app can't wedge waiting on an unresponsive daemon (on Windows the tray
// Quit may be the only way out). Pass null to wait indefinitely, only when the
// user explicitly chose to let post-processing finish.
function signalDaemonShutdown(force: boolean, failsafeMs: number | null): void {
  // Latch both quit flags before the request goes out, so an instant
  // disconnect can't race into the session's reconnect path.
  quitting = true;
  session.beginShutdown();
  session
    .request<string>(force ? Subjects.shutdownForce : Subjects.shutdown)
    .catch(() => {
      /* daemon may already be exiting; we still wait for the disconnect */
    });
  if (failsafeMs !== null && !quitTimer) {
    quitTimer = setTimeout(() => Utils.quit(), failsafeMs);
  }
}

// MARK: Single instance
// Enforce one running desktop app per machine before doing any startup work
// (tray, menu, window, daemon connection). The daemon's own port-bind check only
// guards the daemon; two desktop apps would both connect to the one daemon and
// show two windows (e.g. double-clicking the Windows shortcut). If another
// instance already holds the lock it's asked to surface its window, and we exit.
// See single-instance.ts. Utils.quit force-exits the process, so nothing below
// runs in the second instance.
if (!(await acquireSingleInstanceLock(() => showMainWindow()))) {
  Utils.quit();
}

// MARK: Window

async function getMainViewUrl(): Promise<string> {
  if (appChannel === "dev") {
    try {
      await fetch(DEV_SERVER_URL, { method: "HEAD" });
      console.log(`HMR enabled: Using Vite dev server at ${DEV_SERVER_URL}`);
      return DEV_SERVER_URL;
    } catch {
      console.log(
        "Vite dev server not running. Run 'bun run dev:hmr' for HMR support.",
      );
    }
  }
  return "views://mainview/index.html";
}

// Resolved before the daemon connection starts, so the first update check
// already knows whether this is a dev build.
appChannel = await Updater.localInfo.channel();
const url = await getMainViewUrl();

// mainWindow is the app's single window. Closing it (title-bar X / red button)
// tears the window down but does NOT quit; the app keeps running in the system
// tray (exitOnLastWindowClosed: false in electrobun.config.ts). Reopening from
// the tray recreates the window. The webview is a thin renderer that re-pulls
// full state from the daemon on mount, so the UI is restored.
let mainWindow: BrowserWindow | null = null;

type WindowFrame = { x: number; y: number; width: number; height: number };

// Remembered geometry so a tray reopen restores the user's last position/size
// instead of snapping back to the default frame. Kept current from the live
// window's move/resize events.
let lastFrame: WindowFrame = { x: 200, y: 200, width: 1200, height: 800 };

// Floor for the window: below this the dashboard table's columns and the
// sidebar stop fitting side by side.
const MIN_WINDOW_WIDTH = 900;
const MIN_WINDOW_HEIGHT = 600;

// The frame the resize event reports and the one getFrame/setFrame speak are
// not the same rect (they differ by the title bar), so the clamp below works
// only in the latter and keeps its own history here.
let lastNativeFrame: WindowFrame | null = null;

// Electrobun's BrowserWindow takes no minimum-size option, so we enforce one by
// pushing an undersized window back out. prev is the last frame that cleared
// the floor: when the dragged edge is the origin one, the opposite edge is
// pinned where it was, so the window holds still instead of sliding along with
// the cursor once the limit is hit.
function clampToMinimumSize(frame: WindowFrame, prev: WindowFrame): WindowFrame {
  const width = Math.max(frame.width, MIN_WINDOW_WIDTH);
  const height = Math.max(frame.height, MIN_WINDOW_HEIGHT);
  if (width === frame.width && height === frame.height) return frame;
  return {
    x: width !== frame.width && frame.x !== prev.x ? prev.x + prev.width - width : frame.x,
    y: height !== frame.height && frame.y !== prev.y ? prev.y + prev.height - height : frame.y,
    width,
    height,
  };
}

function enforceMinimumSize(win: BrowserWindow): void {
  const frame = win.getFrame();
  const clamped = clampToMinimumSize(frame, lastNativeFrame ?? frame);
  lastNativeFrame = clamped;
  if (clamped === frame) return;
  // The resulting native resize re-runs this with a frame that already clears
  // the floor, so it settles in one round.
  win.setFrame(clamped.x, clamped.y, clamped.width, clamped.height);
}

function createMainWindow(): void {
  const win = new BrowserWindow({
    title: "VodLabs Recorder",
    url,
    // Passing the remembered frame as its own predecessor floors the size
    // without moving the window.
    frame: clampToMinimumSize(lastFrame, lastFrame),
    rpc,
  });
  lastNativeFrame = null;
  mainWindow = win;
  // Electrobun never gives a Windows window an icon, so set it ourselves — this
  // also settles the class icon every later window inherits. No-op off Windows.
  // See win-window-icon.ts.
  applyWindowIcon(win);
  // Resume pushes for the fresh webview (it also confirms via its own Page
  // Visibility once mounted).
  session.setVisible(true);

  win.on("resize", (event: unknown) => {
    const f = (event as { data?: WindowFrame }).data;
    if (f) lastFrame = { x: f.x, y: f.y, width: f.width, height: f.height };
    enforceMinimumSize(win);
  });
  win.on("move", (event: unknown) => {
    const f = (event as { data?: { x: number; y: number } }).data;
    if (f) lastFrame = { ...lastFrame, x: f.x, y: f.y };
    // A move leaves the clamp's remembered edges stale; drop them so the next
    // undersized resize floors the size without dragging the window back to
    // where it used to be.
    lastNativeFrame = null;
  });
  // Electrobun fires this as the window is destroyed (the native X can't be
  // intercepted to hide instead). Drop the reference and pause pushes; the tray
  // keeps the app reachable until the user picks Quit.
  win.on("close", () => {
    mainWindow = null;
    session.setVisible(false);
  });
}

// showMainWindow raises the existing window or recreates it after a close to the
// tray. Wired to the tray icon/menu and the macOS dock-icon reopen.
function showMainWindow(): void {
  if (mainWindow) {
    if (mainWindow.isMinimized()) mainWindow.unminimize();
    mainWindow.activate();
    return;
  }
  createMainWindow();
}

ApplicationMenu.setApplicationMenu([
  {
    label: "VodLabs Recorder",
    submenu: [
      { role: "about" },
      { type: "separator" },
      { role: "hide" },
      { role: "hideOthers" },
      { role: "showAll" },
      { type: "separator" },
      // A custom action (not role: "quit") so quitting routes through
      // requestQuit for a graceful daemon shutdown. Keeps the standard Cmd+Q.
      {
        label: "Quit VodLabs Recorder",
        action: "quit",
        accelerator: "CmdOrCtrl+Q",
      },
    ],
  },
  {
    label: "Edit",
    submenu: [
      { role: "undo" },
      { role: "redo" },
      { type: "separator" },
      { role: "cut" },
      { role: "copy" },
      { role: "paste" },
      { role: "pasteAndMatchStyle" },
      { role: "delete" },
      { role: "selectAll" },
    ],
  },
  {
    label: "Window",
    submenu: [
      { role: "minimize" },
      { role: "zoom" },
      { type: "separator" },
      { role: "bringAllToFront" },
    ],
  },
]);

// The app-menu Quit (and its Cmd+Q accelerator) is a custom action, so route it
// through requestQuit for a graceful daemon shutdown, same as the tray's Quit.
ApplicationMenu.on("application-menu-clicked", (event: unknown) => {
  const action = (event as { data?: { action?: string } }).data?.action ?? "";
  if (action === "quit") requestQuit();
});

// MARK: Tray
// Keeps the app reachable while the window is closed to the tray. Left-click
// behaviour differs per OS, so the actions live on the menu: Open raises or
// recreates the window, Quit is the explicit exit. (Closing the window never
// quits — the only ways out are this Quit and the app menu's Quit.) The Tray
// constructor degrades gracefully if the platform has no tray.
//
// Tray icon format differs by OS. Windows' native tray loads the icon via
// LoadImage(IMAGE_ICON), which only reads .ico — a .png returns no icon — so
// Windows gets the colour app icon (icon.ico). macOS/Linux take the flat
// silhouette tray.png; on macOS `template: true` tints its alpha to the menu
// bar so it adapts to light/dark. Both assets are bundled to views/ via
// build.copy (see electrobun.config.ts / scripts/gen-icons.sh). The Windows
// `title` also becomes the icon's hover tooltip; macOS would render it as
// menu-bar text, so leave it blank there.
const isWindows = process.platform === "win32";
const tray = new Tray({
  title: isWindows ? "VodLabs Recorder" : "",
  image: isWindows ? "views://icon.ico" : "views://tray.png",
  template: !isWindows,
});
tray.setMenu([
  { type: "normal", label: "Open VodLabs Recorder", action: "open" },
  { type: "separator" },
  { type: "normal", label: "Quit VodLabs Recorder", action: "quit" },
]);
tray.on("tray-clicked", (event: unknown) => {
  const action = (event as { data?: { action?: string } }).data?.action ?? "";
  if (action === "quit") requestQuit();
  else showMainWindow(); // "" = bare icon click, "open" = menu item
});

// macOS: clicking the dock icon while the window is closed reopens it.
app.on("reopen", () => showMainWindow());

// Quit requests from outside our own tray/app-menu actions (e.g. the macOS Dock
// menu's Quit) would hard-exit without shutting the daemon down. before-quit
// fires for every quit path, so veto the first pass through the raw emitter
// (app.on() can't set a response) and reroute it through requestQuit. Once
// `quitting` is set, the second pass goes through.
Electrobun.events.on("before-quit", (event: unknown) => {
  if (quitting) return;
  (event as { response?: { allow: boolean } }).response = { allow: false };
  requestQuit();
});

// Started by the OS at login (see start-at-login.ts): the user did not ask for
// a window, they asked for their streams to be recorded. Come up in the tray
// instead, and let the tray icon open the window when they want it.
if (!process.argv.includes(START_HIDDEN_FLAG)) {
  createMainWindow();
}

console.log("VodLabs Recorder started!");

// Connect in the background; the webview pulls initial state on mount and then
// receives live pushes.
void ensureConnected();

// Start the desktop self-update watcher in the background (primes version/
// channel, schedules the periodic check). Independent of the daemon connection.
void appUpdater.start();

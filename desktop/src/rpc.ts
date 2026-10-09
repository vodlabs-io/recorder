import type { RPCSchema } from "electrobun/rpc";
import type {
  VODDeleteManyResponse,
  VODCancelMovesResponse,
  AddStreamerParams,
  AddStreamerResult,
  CloudIntervals,
  ConnectionInfo,
  DaemonInfo,
  EncodeJob,
  EncodeQueue,
  EncoderList,
  PlatformInfo,
  QuitPrompt,
  SetPlatformLoginParams,
  SetStreamerOptionsParams,
  StreamerRef,
  StreamersSnapshot,
  UpdateState,
  StartAtLogin,
  UserConfig,
  VODListPage,
} from "../../shared/types";

// AppRPCType is the Electrobun RPC schema between the bun process and the
// desktop webview. Desktop-only: the Web UI talks to the daemon
// directly and never sees this schema. The data types it carries live in
// shared/types.ts.
export type AppRPCType = {
  bun: RPCSchema<{
    requests: {
      // Native helpers
      chooseDirectory: {
        params: { startingFolder?: string };
        response: string | null;
      };
      openExternal: { params: { url: string }; response: boolean };
      openPath: { params: { path: string }; response: boolean };
      // Daemon queries
      getConnection: { params: {}; response: ConnectionInfo };
      getSnapshot: { params: {}; response: StreamersSnapshot };
      getPlatforms: { params: {}; response: PlatformInfo[] };
      getCloudIntervals: { params: {}; response: CloudIntervals };
      getInfo: { params: {}; response: DaemonInfo };
      // The desktop app's own log file (the daemon's paths come from getInfo).
      getDesktopLogPath: { params: {}; response: string };
      getConfig: { params: {}; response: UserConfig };
      setConfig: { params: UserConfig; response: UserConfig };
      retryConnect: { params: {}; response: ConnectionInfo };
      // Start at login. The response is what the OS holds after the change,
      // which is not always what was asked for — see desktop/src/bun/start-at-login.
      getStartAtLogin: { params: {}; response: StartAtLogin };
      setStartAtLogin: { params: { enabled: boolean }; response: StartAtLogin };
      // Third-party platform logins (Accounts page). PRIVACY: platformLogin opens
      // a webview to the platform's login page, captures the resulting cookies
      // (incl. HttpOnly) from that webview's own session, and hands them to the
      // LOCAL daemon — they are never sent to the vodlabs cloud. Resolves true if
      // a login was captured, false if the user closed the window first.
      // setPlatformCookies is the advanced paste path; signOutPlatform removes the
      // stored login and clears the login webview's session data.
      platformLogin: { params: { platform: string }; response: boolean };
      setPlatformCookies: { params: SetPlatformLoginParams; response: boolean };
      signOutPlatform: { params: { platform: string }; response: boolean };
      // Updates (app + daemon as one unit). getUpdateState pulls the current
      // state on mount; checkUpdates forces a re-check of both halves (each
      // auto-downloads what it finds, with progress arriving via the
      // updateStateChanged push); installUpdates restarts the daemon onto its
      // new binary and then installs the app build and relaunches (so it
      // resolves just before the process exits, if the app half had anything).
      getUpdateState: { params: {}; response: UpdateState };
      checkUpdates: { params: {}; response: UpdateState };
      installUpdates: { params: {}; response: boolean };
      // Quit flow. getQuitPrompt lets a freshly-opened window pull a pending
      // prompt it missed (the push below may fire before the webview mounts).
      // confirm = graceful shutdown, force = skip pending processing, cancel =
      // keep running. Each resolves true once acted on.
      getQuitPrompt: { params: {}; response: QuitPrompt | null };
      confirmQuit: { params: {}; response: boolean };
      forceQuit: { params: {}; response: boolean };
      cancelQuit: { params: {}; response: boolean };
      // Streamer mutations (resolve to a status string from the daemon)
      addStreamer: { params: AddStreamerParams; response: AddStreamerResult };
      removeStreamer: { params: StreamerRef; response: string };
      removeAllStreamers: { params: {}; response: string };
      disableStreamer: { params: StreamerRef; response: string };
      disableAllStreamers: { params: {}; response: string };
      enableStreamer: { params: StreamerRef; response: string };
      enableAllStreamers: { params: {}; response: string };
      checkStreamer: { params: StreamerRef; response: string };
      setStreamerOptions: { params: SetStreamerOptionsParams; response: string };
      // VOD Archive. listVodsPage proxies one page of the daemon's listing
      // (the paging loop lives in the webview, so it can render pages as they
      // arrive); getRecordingThumbnail resolves to base64 JPEG bytes.
      listVodsPage: {
        params: { offset: number; limit: number; rescan?: boolean };
        response: VODListPage;
      };
      getRecordingThumbnail: {
        params: { dir: string; file: string };
        response: string;
      };
      deleteVOD: { params: { dir: string }; response: string };
      deleteVODs: {
        params: { dirs: string[] };
        response: VODDeleteManyResponse;
      };
      cancelMoves: {
        params: { platform?: string; username?: string };
        response: VODCancelMovesResponse;
      };
      // Encode queue (VOD file merging): plain daemon proxies. Live queue
      // rev/progress ride the streamersChanged snapshot (snapshot.encode).
      enqueueMerge: {
        params: { dir: string; deleteParts: boolean };
        response: EncodeJob;
      };
      listEncodeQueue: { params: {}; response: EncodeQueue };
      removeEncodeJob: { params: { id: number }; response: boolean };
      reorderEncodeJob: {
        params: { id: number; direction: "up" | "down" | "top" };
        response: boolean;
      };
      cancelEncodeJob: { params: { id: number }; response: boolean };
      retryEncodeJob: { params: { id: number }; response: boolean };
      setEncodeQueuePaused: { params: { paused: boolean }; response: boolean };
      listEncoders: { params: { refresh?: boolean }; response: EncoderList };
    };
    // Webview → bun. The page's visibility (Page Visibility API) changed. While
    // the window is hidden the OS suspends the webview and buffers pushes, so bun
    // pauses streamer pushes while hidden and flushes once on becoming visible.
    messages: {
      viewVisibilityChanged: { visible: boolean };
    };
  }>;
  webview: RPCSchema<{
    requests: {};
    // Bun → webview push. A side's `messages` are the ones it RECEIVES, so these
    // live under `webview` and the bun process emits them via rpc.send.*.
    messages: {
      streamersChanged: StreamersSnapshot;
      connectionChanged: ConnectionInfo;
      // Bun asks the webview to show the quit dialog (daemon has work in flight).
      quitRequested: QuitPrompt;
      // Live update state for both halves plus the install sequence
      // (checking → downloading → ready → restarting-daemon → installing-app).
      updateStateChanged: UpdateState;
      // The daemon's VOD Archive contents changed (rides the daemon.vods_rev
      // KV key); an open VOD Archive screen re-pulls the listing.
      recordingsChanged: {};
    };
  }>;
};

// Transport-agnostic daemon client interface. UI code depends only on this;
// the Electrobun implementation (electrobun-client.ts) wires it to the bun
// process over RPC, and the Web UI's (web-client.ts) speaks NATS over WebSocket.

export type {
  PlatformLoginView,
  Activity,
  AddStreamerParams,
  AddStreamerResult,
  AppUpdateStatus,
  CloudInterval,
  CloudIntervals,
  ConnectionInfo,
  ConnectionState,
  DaemonInfo,
  DaemonMeta,
  DiskStatus,
  FolderSuggestion,
  SpaceState,
  StorageRoot,
  DaemonUpdateStatus,
  EncodeJob,
  EncodeProgress,
  EncodeQueue,
  EncodeState,
  EncoderList,
  EncoderStatus,
  InstallPhase,
  LiveStatus,
  MoveFailureReason,
  PlatformInfo,
  LoginFlow,
  QuitPrompt,
  RecordingStats,
  SetPlatformLoginParams,
  SetStreamerOptionsParams,
  StreamerRef,
  StreamerView,
  StreamersSnapshot,
  UpdatePhase,
  UpdateResult,
  UpdateState,
  UserConfig,
  ErrorReportsState,
  BugReportRequest,
  BugReportResult,
  VODFile,
  VODListPage,
  VODPreviews,
  RecordingListResult,
  VOD,
  VODState,
  VODDeleteManyResponse,
  VODCancelMovesResponse,
  VODDeleteResult,
} from "@shared/types";

import type {
  FolderSuggestion,
  StartAtLogin,
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
  UserConfig,
  ErrorReportsState,
  BugReportRequest,
  BugReportResult,
  VODListPage,
  VODPreviews,
} from "@shared/types";

// DaemonCapabilities declares which host-dependent features a DaemonClient
// implementation actually supports, so the UI can hide what the host can't do.
// The desktop app supports all of them; the Web UI none (it has no
// native dialogs or file manager, doesn't own the daemon's lifecycle, and
// updates by reloading the page).
export type DaemonCapabilities = {
  // Native folder picker (Settings → Files). Without it, path fields are
  // free-text inputs.
  chooseDirectory: boolean;
  // Reveal a folder/file in the OS file manager (Settings → About).
  openPath: boolean;
  // Graceful-quit flow (quit dialog + confirm/force/cancel).
  quit: boolean;
  // Host app self-update (the Update tab's application row).
  appUpdate: boolean;
  // Host can download updates and install them (restart the daemon onto its new
  // binary, relaunch itself). Without it the Update tab is read-only: it still
  // reports the versions and whether one is behind, but shows no button — the
  // Web UI owns neither the daemon's lifecycle nor its own build.
  installUpdates: boolean;
  // In-app webview sign-in for third-party platform logins (Accounts page).
  // Desktop only — a browser can't read another site's cookies, so the Web UI
  // offers only the "paste a Cookie header" path instead.
  webviewLogin: boolean;
  // Signed in to the daemon with its sign-in token (the Web UI), so the UI
  // offers Sign out.
  webSession: boolean;
  // Recordings play and download in the page through mediaUrl, for hosts
  // whose recordings live on another machine (the Web UI).
  browserMedia: boolean;
};

export interface DaemonClient {
  readonly capabilities: DaemonCapabilities;

  // Queries
  getConnection(): Promise<ConnectionInfo>;
  getSnapshot(): Promise<StreamersSnapshot>;
  getPlatforms(): Promise<PlatformInfo[]>;
  getCloudIntervals(): Promise<CloudIntervals>;
  getInfo(): Promise<DaemonInfo>;
  // Path to the host app's own log file, for the About screen to list beside
  // the daemon's. "" on hosts that don't write one (the Web UI).
  getDesktopLogPath(): Promise<string>;

  // Updates, app and daemon as one unit. getUpdateState pulls current state on
  // mount; checkUpdates re-checks both halves (each auto-downloads what it
  // finds, with progress arriving via onUpdateStateChanged); installUpdates
  // restarts the daemon onto its new binary and then installs the app build and
  // relaunches. Only meaningful when capabilities.installUpdates.
  getUpdateState(): Promise<UpdateState>;
  checkUpdates(): Promise<UpdateState>;
  installUpdates(): Promise<boolean>;

  getConfig(): Promise<UserConfig>;
  setConfig(cfg: UserConfig): Promise<UserConfig>;

  // Error reports (off until the user turns them on) and bug reports (sent
  // only when the user sends one). Both go from the daemon to Sentry.
  getErrorReports(): Promise<ErrorReportsState>;
  setErrorReports(enabled: boolean): Promise<ErrorReportsState>;
  sendBugReport(req: BugReportRequest): Promise<BugReportResult>;
  // Folders mounted into a container daemon, offered as suggestions in the
  // folder settings. Empty for any other daemon.
  getFolderSuggestions(): Promise<FolderSuggestion[]>;
  retryConnect(): Promise<ConnectionInfo>;

  // vodlabs.io account. startCloudSignIn mints a link code and returns the
  // dashboard URL to open in the browser (the daemon then polls for the
  // confirmation in the background); live link state arrives via the snapshot

  // Third-party platform logins (Accounts page) for recording login-restricted
  // streams. PRIVACY: logins are captured and stored locally and NEVER sent to
  // the vodlabs cloud. platformLogin runs the desktop webview sign-in flow
  // (capability webviewLogin only) and resolves true once a login was captured,
  // false if the user closed the window without signing in. setPlatformCookies
  // is the advanced paste path available everywhere. signOutPlatform removes the
  // stored login (and, on desktop, clears the login webview's own session data).
  // All three resolve once the daemon has stored/removed the login; the resulting
  // account status arrives via the snapshot (snapshot.accounts).
  platformLogin(platform: string): Promise<boolean>;
  setPlatformCookies(params: SetPlatformLoginParams): Promise<void>;
  signOutPlatform(platform: string): Promise<void>;

  // VOD Archive. listVodsPage returns one page of the daemon's listing; the
  // paging loop (progressive render, rev-conditional refresh) lives in the UI
  // via @shared/daemon/recordings.fetchRecordings. getRecordingThumbnail resolves
  // to base64 JPEG bytes (generated on demand when a VOD has no sidecar
  // screenshot); deleteVOD removes a whole session folder. Opening a
  // VOD in the OS player and revealing its folder both go through openPath
  // (capability-gated: desktop only).
  listVodsPage(offset: number, limit: number, rescan?: boolean): Promise<VODListPage>;
  getRecordingThumbnail(dir: string, file: string): Promise<string>;
  // getVodPreviews resolves to one file's preview layout and hover strip,
  // getContactSheet to its contact sheet as base64 JPEG bytes. Both are
  // generated on first request.
  getVodPreviews(dir: string, file: string): Promise<VODPreviews>;
  getContactSheet(dir: string, file: string): Promise<string>;
  deleteVOD(dir: string): Promise<string>;
  // deleteVODs removes a selection in one call. Not a loop over deleteVOD:
  // each single delete bumps the archive rev (refetching the list underneath
  // the user), and partial failure is normal, so the batch reports per-entry
  // outcomes instead of failing as a whole.
  deleteVODs(dirs: string[]): Promise<VODDeleteManyResponse>;
  // cancelMoves stops queued moves from retrying — for one streamer, or for
  // every pending move when no key is given. Files already at the destination
  // stay there, so a part-moved session is left split across the download and
  // archive folders, and a cancelled session can't be re-queued. Callers warn
  // before asking.
  cancelMoves(key?: {
    platform: string;
    username: string;
  }): Promise<VODCancelMovesResponse>;

  // Encode queue (VOD file merging). enqueueMerge queues a merge of a VOD's
  // numbered VOD files; deleteParts=false keeps the originals as .bak.
  // Queue changes announce themselves via snapshot.encode.queueRev (re-pull
  // the list) and the running job's progress via snapshot.encode.progress.
  // LOCAL-ONLY: these subjects are never bridged to the Dashboard.
  enqueueMerge(dir: string, deleteParts: boolean): Promise<EncodeJob>;
  // enqueueConvert queues a re-encode of a VOD's single vod.webm to an H.264
  // vod.mp4; deleteOriginal=false keeps vod.webm as .bak.
  enqueueConvert(dir: string, deleteOriginal: boolean): Promise<EncodeJob>;
  listEncodeQueue(): Promise<EncodeQueue>;
  removeEncodeJob(id: number): Promise<void>;
  reorderEncodeJob(id: number, direction: "up" | "down" | "top"): Promise<void>;
  cancelEncodeJob(id: number): Promise<void>;
  retryEncodeJob(id: number): Promise<void>;
  setEncodeQueuePaused(paused: boolean): Promise<void>;
  // listEncoders reports encoder availability/self-test status for the
  // Settings dropdown; refresh forces a re-test (self-tests cost ~a second).
  listEncoders(refresh?: boolean): Promise<EncoderList>;

  // Streamer mutations (resolve to a daemon status string)
  addStreamer(params: AddStreamerParams): Promise<AddStreamerResult>;
  removeStreamer(ref: StreamerRef): Promise<string>;
  removeAllStreamers(): Promise<string>;
  disableStreamer(ref: StreamerRef): Promise<string>;
  disableAllStreamers(): Promise<string>;
  enableStreamer(ref: StreamerRef): Promise<string>;
  enableAllStreamers(): Promise<string>;
  // checkStreamer asks for a streamer's status now. On a retired streamer the
  // daemon upgrades this to a re-check that also clears the retirement, which
  // is the only way back from "defunct" — so it can take a few seconds.
  checkStreamer(ref: StreamerRef): Promise<string>;
  setStreamerOptions(params: SetStreamerOptionsParams): Promise<string>;

  // Native helpers
  chooseDirectory(startingFolder?: string): Promise<string | null>;
  openExternal(url: string): Promise<boolean>;
  openPath(path: string): Promise<boolean>;

  // Whether the OS starts the recorder at login. Reading it goes to the OS
  // rather than to a setting of ours, because the user can change it there;
  // setting it reports back what the OS holds afterwards. Hosts that cannot
  // ask (the daemon has no login items of its own) report unsupported.
  getStartAtLogin(): Promise<StartAtLogin>;
  setStartAtLogin(enabled: boolean): Promise<StartAtLogin>;

  // URL that plays (or, with download, saves) one file of a finished
  // recording. "" unless capabilities.browserMedia.
  mediaUrl(dir: string, file: string, download?: boolean): string;
  // URL of one file's timeline-preview track (WebVTT) for a player's seek
  // bar. "" unless capabilities.browserMedia.
  previewTrackUrl(dir: string, file: string): string;

  // Ends this browser's Web UI session and goes to the sign-in page. Only
  // meaningful when capabilities.webSession.
  signOut(): Promise<void>;

  // Quit flow. getQuitPrompt pulls a pending prompt the window may have missed
  // on mount; confirm = graceful shutdown, force = skip pending processing,
  // cancel = keep running.
  getQuitPrompt(): Promise<QuitPrompt | null>;
  confirmQuit(): Promise<boolean>;
  forceQuit(): Promise<boolean>;
  cancelQuit(): Promise<boolean>;

  // Push subscriptions (return an unsubscribe fn)
  onStreamersChanged(cb: (snapshot: StreamersSnapshot) => void): () => void;
  onConnectionChanged(cb: (info: ConnectionInfo) => void): () => void;
  onQuitRequested(cb: (prompt: QuitPrompt) => void): () => void;
  onUpdateStateChanged(cb: (state: UpdateState) => void): () => void;
  // Fired when the daemon's VOD Archive contents changed (VOD produced,
  // moved, or deleted); an open VOD Archive screen re-pulls the listing.
  onVodsChanged(cb: () => void): () => void;
}

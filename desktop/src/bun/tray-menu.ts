import { expiredLogins } from "../../../shared/daemon/logins";
import { countPendingWork } from "../../../shared/daemon/pending-work";
import type {
  ConnectionInfo,
  EncodeQueue,
  StorageRoot,
  StreamersSnapshot,
  StreamerView,
  UpdateState,
} from "../../../shared/types";

// MARK: Tray menu
// ---------------------------------------------------------------------------
// What the tray menu says, as a pure function of the app's state: anything
// needing the user's attention first, then the current figures, then the quick
// actions. Every status row opens the screen that deals with it.
// ---------------------------------------------------------------------------

export const TRAY_OPEN = "open";
export const TRAY_ADD_STREAMER = "add-streamer";
export const TRAY_OPEN_RECORDINGS = "open-recordings";
export const TRAY_PAUSE_ENCODING = "pause-encoding";
export const TRAY_RESUME_ENCODING = "resume-encoding";
export const TRAY_INSTALL_UPDATE = "install-update";
export const TRAY_QUIT = "quit";
const ROUTE_PREFIX = "route:";

// trayActionRoute is the screen a status row opens, or null for any other action.
export function trayActionRoute(action: string): string | null {
  return action.startsWith(ROUTE_PREFIX) ? action.slice(ROUTE_PREFIX.length) : null;
}

const DASHBOARD = "#/dashboard";
const ENCODING = "#/encoding";
const ACCOUNTS = "#/accounts";
const FILES = "#/settings/files";
const UPDATE = "#/update";

// Keeps the end of a path, the part that tells folders apart, at a width a menu
// can show.
const MAX_PATH_LABEL = 40;
const MAX_NAMED_LOGINS = 3;

export type TrayMenuItem =
  | { type: "normal"; label: string; action: string; enabled?: boolean }
  | { type: "separator" };

export type TrayMenuInput = {
  connection: ConnectionInfo;
  snapshot: StreamersSnapshot;
  update: UpdateState;
  // The last encode queue listing; null until the daemon has answered one.
  encodeQueue: EncodeQueue | null;
  // Display names by platform key. A platform missing here shows its key.
  platformNames: ReadonlyMap<string, string>;
  // False when the recordings aren't on this machine's filesystem as the daemon
  // names them (a containerized daemon's paths are its own).
  canOpenRecordings: boolean;
  // Whether rows may carry figures that change every few seconds, which rebuilds
  // the menu just as often. Only some platforms can replace a menu while it is
  // open (see the MARK: Tray comment in index.ts).
  liveFigures: boolean;
};

export type TrayMenu = {
  items: TrayMenuItem[];
  // One line for the icon's hover tooltip, where the platform has one.
  tooltip: string;
  // How many rows ask something of the user, for the icon's badge and tooltip.
  attention: number;
};

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

function formatBytes(bytes: number): string {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = Math.max(0, bytes);
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  return `${value.toFixed(value >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function pathLabel(path: string): string {
  if (path.length <= MAX_PATH_LABEL) return path;
  return `…${path.slice(-(MAX_PATH_LABEL - 1))}`;
}

function rootLabel(root: StorageRoot): string {
  return pathLabel(root.paths[0] ?? "(unset)");
}

function connectionRow(connection: ConnectionInfo): TrayMenuItem | null {
  const row = (label: string): TrayMenuItem => ({
    type: "normal",
    label,
    action: ROUTE_PREFIX + DASHBOARD,
  });
  switch (connection.state) {
    case "connected":
      return null;
    case "connecting":
      return row("Connecting to the recorder…");
    case "downloading":
      return row("Downloading the recorder…");
    case "no-binary":
      return row("⚠ Recorder is missing");
    case "disconnected":
      return row("⚠ Not connected to the recorder");
  }
}

// attentionRows lists what the user has to deal with, most urgent first.
function attentionRows({
  snapshot,
  update,
  encodeQueue,
  platformNames,
  liveFigures,
}: TrayMenuInput): TrayMenuItem[] {
  const rows: TrayMenuItem[] = [];
  const add = (label: string, route: string) =>
    rows.push({ type: "normal", label: `⚠ ${label}`, action: ROUTE_PREFIX + route });
  const { daemon, streamers, accounts } = snapshot;

  if (daemon.updateRequired) add("Update required to keep recording", UPDATE);
  if (daemon.blocked) {
    add("Blocked from the VodLabs service", DASHBOARD);
  } else if (daemon.registration && !daemon.registration.registered) {
    add("Can't register with the VodLabs service", DASHBOARD);
  } else if (daemon.service && !daemon.service.reachable) {
    add("Can't reach the VodLabs service", DASHBOARD);
  } else if (daemon.service?.subscribe_error) {
    add("New streamers aren't being checked yet", DASHBOARD);
  }

  const blocked = streamers.filter((s) => s.activity === "blocked_no_space").length;
  if (blocked > 0) {
    add(`${plural(blocked, "live streamer")} not recording: no space`, FILES);
  }
  const roots = daemon.diskStatus?.roots ?? [];
  for (const r of roots) {
    if (r.state === "full") add(`Out of space: ${rootLabel(r)}`, FILES);
  }
  for (const r of roots) {
    if (r.state === "unknown") add(`Folder unavailable: ${rootLabel(r)}`, FILES);
  }
  for (const r of roots) {
    if (r.state !== "low") continue;
    const free = liveFigures ? ` (${formatBytes(r.usage.free_bytes)} free)` : "";
    add(`Low disk space: ${rootLabel(r)}${free}`, FILES);
  }

  const moving = streamers.filter((s) => s.activity === "moving_retrying").length;
  if (moving > 0) {
    add(`${plural(moving, "recording")} can't be moved to the archive`, FILES);
  }

  const expired = expiredLogins(accounts);
  if (expired.length > 0) {
    const names = expired.map((a) => platformNames.get(a.platform) ?? a.platform);
    const shown = names.slice(0, MAX_NAMED_LOGINS).join(", ");
    const more = names.length - MAX_NAMED_LOGINS;
    add(
      `${plural(names.length, "login")} expired: ${shown}${more > 0 ? ` +${more} more` : ""}`,
      ACCOUNTS,
    );
  }
  const signIn = streamers.filter((s) => s.liveStatus === "auth_required").length;
  if (signIn > 0) {
    add(`${signIn === 1 ? "1 streamer needs" : `${signIn} streamers need`} a sign-in`, ACCOUNTS);
  }

  const failed = encodeQueue?.jobs.filter((j) => j.status === "failed").length ?? 0;
  if (failed > 0) add(`${plural(failed, "encoding job")} failed`, ENCODING);

  if (update.install.phase === "error") add("Update didn't install", UPDATE);
  const updateItem = updateRow(update);
  if (updateItem) rows.push(updateItem);
  return rows;
}

// updateRow covers the same versions the Update tab's badge does, and offers the
// install under the same rule as the tab's button: something is ready and neither
// half is still settling, so a pair mid-download can't be installed twice.
function updateRow({ app, daemon, install }: UpdateState): TrayMenuItem | null {
  const row = (label: string, action: string): TrayMenuItem => ({ type: "normal", label, action });
  if (install.phase === "restarting-daemon" || install.phase === "installing-app") {
    return row("Installing update…", ROUTE_PREFIX + UPDATE);
  }
  const version = app.availableVersion ?? daemon.availableVersion;
  if (version === null) return null;
  const halves = [app, daemon];
  const settling = halves.some((h) => h.phase === "checking" || h.phase === "downloading");
  return !settling && halves.some((h) => h.phase === "ready")
    ? row(`Install Update and Restart: ${version}`, TRAY_INSTALL_UPDATE)
    : row(`Update available: ${version}`, ROUTE_PREFIX + UPDATE);
}

// isLive matches when the dashboard calls a streamer live: one with both
// monitoring and recording off is no longer checked, so its status is stale.
function isLive(s: StreamerView): boolean {
  return s.liveStatus === "live" && (s.monitor || s.record);
}

function isRecordingNow(s: StreamerView): boolean {
  return s.activity === "recording" || s.activity === "lagging" || s.activity === "paused";
}

function recordingLabel(streamers: StreamerView[], liveFigures: boolean): string {
  const { recording } = countPendingWork(streamers);
  if (recording === 0) return "Not recording";
  const label = `${recording} recording`;
  if (!liveFigures) return label;
  return `${label} · ${formatSpeed(streamers)}`;
}

function formatSpeed(streamers: StreamerView[]): string {
  const total = streamers.reduce((sum, s) => sum + (s.stats?.speed ?? 0), 0);
  return `${formatBytes(total)}/s`;
}

function liveLabel(streamers: StreamerView[]): string {
  const live = streamers.filter(isLive);
  if (live.length === 0) return "No streamers live";
  const notRecording = live.filter((s) => !isRecordingNow(s)).length;
  return notRecording > 0 ? `${live.length} live, ${notRecording} not recording` : `${live.length} live`;
}

function encodingLabel(
  { pending, progress }: StreamersSnapshot["encode"],
  paused: boolean,
  liveFigures: boolean,
): string {
  if (pending === 0) return paused ? "Encoding queue empty · paused" : "Encoding queue empty";
  const queued = pending - (progress ? 1 : 0);
  if (!progress) return paused ? `Encoding paused · ${queued} queued` : `${queued} queued to encode`;
  const percent = liveFigures && progress.percent >= 0 ? ` ${Math.round(progress.percent)}%` : "";
  const parts = [`Encoding${percent}`];
  if (queued > 0) parts.push(`${queued} queued`);
  if (paused) parts.push("paused");
  return parts.join(" · ");
}

function statRows({ snapshot, encodeQueue, liveFigures }: TrayMenuInput): TrayMenuItem[] {
  const { streamers, encode } = snapshot;
  const row = (label: string, route: string): TrayMenuItem => ({
    type: "normal",
    label,
    action: ROUTE_PREFIX + route,
  });
  const rows = [
    row(recordingLabel(streamers, liveFigures), DASHBOARD),
    row(liveLabel(streamers), DASHBOARD),
  ];
  const { processing } = countPendingWork(streamers);
  if (processing > 0) rows.push(row(`${processing} processing or moving`, DASHBOARD));
  rows.push(row(encodingLabel(encode, encodeQueue?.paused ?? false, liveFigures), ENCODING));
  return rows;
}

function tooltip(input: TrayMenuInput, attention: number): string {
  const name = "VodLabs Recorder";
  if (input.connection.state !== "connected") return name;
  const { streamers } = input.snapshot;
  const { recording } = countPendingWork(streamers);
  const parts = [
    recording > 0 ? `${recording} recording at ${formatSpeed(streamers)}` : "not recording",
    `${streamers.filter(isLive).length} live`,
  ];
  if (attention > 0) parts.push(`${attention} ${attention === 1 ? "needs" : "need"} attention`);
  return `${name}: ${parts.join(", ")}`;
}

// encodingAction pauses a queue with work in it. A paused queue can always be
// resumed, even empty, because it would otherwise hold back the next job added.
function encodingAction({ snapshot, encodeQueue }: TrayMenuInput): TrayMenuItem[] {
  if (encodeQueue?.paused) {
    return [{ type: "normal", label: "Resume Encoding", action: TRAY_RESUME_ENCODING }];
  }
  return snapshot.encode.pending > 0
    ? [{ type: "normal", label: "Pause Encoding", action: TRAY_PAUSE_ENCODING }]
    : [];
}

export function buildTrayMenu(input: TrayMenuInput): TrayMenu {
  const connected = input.connection.state === "connected";
  const status = connectionRow(input.connection);
  const rows = connected ? attentionRows(input) : status ? [status] : [];
  const stats = connected ? statRows(input) : [];

  const separator: TrayMenuItem = { type: "separator" };
  const items: TrayMenuItem[] = [
    ...rows,
    ...(rows.length > 0 ? [separator] : []),
    ...stats,
    ...(stats.length > 0 ? [separator] : []),
    { type: "normal", label: "Open VodLabs Recorder", action: TRAY_OPEN },
    // These need the daemon, so don't promise them while there is none.
    { type: "normal", label: "Add Streamer…", action: TRAY_ADD_STREAMER, enabled: connected },
    ...(input.canOpenRecordings
      ? [{ type: "normal" as const, label: "Open Recordings Folder", action: TRAY_OPEN_RECORDINGS, enabled: connected }]
      : []),
    ...(connected ? encodingAction(input) : []),
    separator,
    { type: "normal", label: "Quit VodLabs Recorder", action: TRAY_QUIT },
  ];
  const attention = rows.filter(needsUser).length;
  return { items, attention, tooltip: tooltip(input, attention) };
}

function needsUser(item: TrayMenuItem): boolean {
  return (
    item.type === "normal" && (item.label.startsWith("⚠") || item.action === TRAY_INSTALL_UPDATE)
  );
}

import { Updater, Utils } from "electrobun/bun";
import { mkdirSync } from "fs";
import { join } from "path";

// MARK: Update notifications
// ---------------------------------------------------------------------------
// A native OS notification, at most once per (kind, version), so a tray-resident
// user with no open window finds out an update is waiting — and doesn't get
// re-nagged about the same version on every restart. Shared by the app and
// daemon updaters; the last-notified version per kind is persisted next to the
// app's data.
// ---------------------------------------------------------------------------

// NotifyKind keys the debounce, so an app update and a daemon update that happen
// to share a version number don't suppress each other.
export type NotifyKind = "app" | "daemon";

type NotifyState = Partial<Record<NotifyKind, string>>;

let cached: NotifyState | null = null;

async function filePath(): Promise<string> {
  const dir = await Updater.appDataFolder();
  mkdirSync(dir, { recursive: true });
  return join(dir, "update-notify.json");
}

async function load(): Promise<NotifyState> {
  if (cached) return cached;
  try {
    cached = (await Bun.file(await filePath()).json()) as NotifyState;
  } catch {
    cached = {}; // file absent / unreadable on first run
  }
  return cached;
}

async function save(state: NotifyState): Promise<void> {
  cached = state;
  try {
    await Bun.write(await filePath(), JSON.stringify(state));
  } catch (err) {
    console.error("persisting update-notify failed:", err);
  }
}

/**
 * notifyOnce shows a native notification for (kind, version) unless one has
 * already been shown for that exact pair. Best-effort: a failure to notify never
 * blocks the update itself.
 */
export async function notifyOnce(
  kind: NotifyKind,
  version: string,
  opts: { title: string; body: string },
): Promise<void> {
  if (!version) return;
  const state = await load();
  if (state[kind] === version) return;
  state[kind] = version;
  await save(state);
  try {
    Utils.showNotification({ title: opts.title, body: opts.body });
  } catch (err) {
    console.error("showNotification failed:", err);
  }
}

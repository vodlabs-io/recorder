import { Updater } from "electrobun/main";
import type { AppUpdateStatus } from "../../../shared/types";
import { devBuildInfo, devBuildMessage } from "./dev-build";
import { notifyOnce } from "./update-notify";

// MARK: App self-update
// ---------------------------------------------------------------------------
// AppUpdater wraps Electrobun's Updater (hash-based BSDIFF self-update) for the
// desktop app — one half of the Update tab; DaemonUpdater owns the other. It
// checks on a light background loop, auto-downloads a found build so installing
// is one click, and fires a debounced native notification once the build is
// ready to install.
//
// All of this must live in the bun process: Electrobun's Updater only exists in
// electrobun/bun, and the bun process outlives the webview (which is destroyed
// when the window closes to the tray, so it can't be the one to notify).
// ---------------------------------------------------------------------------

// Background cadence. STARTUP_DELAY_MS gives launch a moment before the first
// check; CHECK_INTERVAL_MS is the steady-state re-check. The Update tab also
// calls check() on mount, so opening it never shows a stale answer.
const STARTUP_DELAY_MS = 10_000;
const CHECK_INTERVAL_MS = 6 * 60 * 60 * 1000; // 6h

function emptyStatus(): AppUpdateStatus {
  return {
    phase: "idle",
    currentVersion: "",
    channel: "",
    availableVersion: null,
    progress: null,
    message: null,
    build: null,
  };
}

export class AppUpdater {
  private status: AppUpdateStatus = emptyStatus();
  // Pushed on every status change (set by index.ts, which folds both halves
  // into one UpdateState for the webview).
  private onChange: () => void = () => {};
  // Coalesces an overlapping manual check and periodic tick into one run.
  private checking: Promise<AppUpdateStatus> | null = null;

  setOnChange(cb: () => void): void {
    this.onChange = cb;
  }

  getStatus(): AppUpdateStatus {
    return this.status;
  }

  // isReady reports whether a downloaded build is waiting to be installed, so
  // the install sequence knows whether to call apply() at all — applyUpdate() is
  // a silent no-op when nothing was downloaded.
  isReady(): boolean {
    return this.status.phase === "ready";
  }

  private set(patch: Partial<AppUpdateStatus>): void {
    this.status = { ...this.status, ...patch };
    try {
      this.onChange();
    } catch {
      /* transport not ready; the webview pulls state on mount */
    }
  }

  // setDevBuild reads the checkout again on every check, so the Update tab
  // follows commits made while the app runs.
  private setDevBuild(): void {
    const build = devBuildInfo();
    this.set({ phase: "disabled", build, message: devBuildMessage(build) });
  }

  // start primes the local version/channel, wires Electrobun's status stream for
  // download progress, and — unless this is a dev build (updates disabled) —
  // schedules the first check and the periodic loop.
  async start(): Promise<void> {
    let local: { version: string; channel: string };
    try {
      local = await Updater.getLocalInfo();
    } catch {
      local = { version: "", channel: "" };
    }
    this.set({ currentVersion: local.version, channel: local.channel });

    // Electrobun emits a granular status stream; we only lift download progress
    // out of it. The imperative check()/download() flow owns the major phases, so
    // there's no fight over the phase field.
    Updater.onStatusChange((entry) => {
      if (
        entry.status === "download-progress" &&
        entry.details?.progress != null
      ) {
        this.set({
          phase: "downloading",
          progress: Math.round(entry.details.progress),
        });
      }
    });

    if (local.channel === "dev") {
      this.setDevBuild();
      return;
    }

    setTimeout(() => void this.check(), STARTUP_DELAY_MS);
    setInterval(() => void this.check(), CHECK_INTERVAL_MS);
  }

  // check runs one desktop-update check, auto-downloading when a newer build is
  // found and firing a debounced notification. Concurrent callers share one run.
  check(): Promise<AppUpdateStatus> {
    if (this.checking) return this.checking;
    this.checking = this.runCheck().finally(() => {
      this.checking = null;
    });
    return this.checking;
  }

  private async runCheck(): Promise<AppUpdateStatus> {
    if (this.status.channel === "dev") {
      this.setDevBuild();
      return this.status;
    }
    // Already downloaded and waiting to install: nothing left to learn, and the
    // Update tab re-checks on every mount, which must not replay the download.
    if (this.status.phase === "ready") return this.status;
    // Only advertise "checking" when there's no settled answer worth keeping on
    // screen. A background re-check shouldn't make a steady "Up to date" flicker.
    if (this.status.phase === "idle" || this.status.phase === "error") {
      this.set({ phase: "checking", message: null });
    }
    try {
      const res = await Updater.checkForUpdate();
      if (res.error) {
        this.set({ phase: "error", message: res.error, progress: null });
        return this.status;
      }
      if (!res.updateAvailable) {
        this.set({ phase: "up-to-date", availableVersion: null, progress: null });
        return this.status;
      }
      // A newer build exists. Download it now so installing later is one click.
      this.set({
        phase: "downloading",
        availableVersion: res.version,
        progress: null,
      });
      await Updater.downloadUpdate();
      this.set({ phase: "ready", progress: null });
      void notifyOnce("app", res.version, {
        title: "Update ready to install",
        body: `VodLabs Recorder ${res.version} is ready. Open the app → Update to install it.`,
      });
      return this.status;
    } catch (err) {
      this.set({
        phase: "error",
        message: err instanceof Error ? err.message : String(err),
        progress: null,
      });
      return this.status;
    }
  }

  // apply installs the downloaded build and relaunches the app. Only meaningful
  // when isReady() — Electrobun's applyUpdate() silently does nothing otherwise.
  // The process exits mid-call, so the resolved value rarely reaches the caller.
  async apply(): Promise<boolean> {
    await Updater.applyUpdate();
    return true;
  }
}

import { devBuildMessage } from "./dev-build";
import type { DaemonUpdateStatus } from "../../../shared/types";
import {
  DEFAULT_BIN_REPO,
  compareVersionsDesc,
  downloadDaemon,
  latestDaemonVersion,
} from "./daemon-process";
import { notifyOnce } from "./update-notify";

// binRepo is where daemon releases are published, overridable for testing.
function binRepo(): string {
  return process.env.VLREC_BIN_REPO ?? DEFAULT_BIN_REPO;
}

// ---------------------------------------------------------------------------
// DaemonUpdater is the daemon half of the Update tab, mirroring AppUpdater.
//
// The bun process owns this rather than the daemon itself for three reasons:
// the daemon's own update RPC downloads synchronously inside a NATS request
// that has a 5s timeout (so a 20-40 MB binary always "fails"); downloadDaemon()
// here already reports byte progress; and the bun process is the thing that
// spawns and shuts the daemon down, so it knows exactly which binary it will
// launch next.
//
// The version to install is the cloud's recommended_version, which reaches us
// through the daemon's heartbeat → daemon.upstream_version KV key → snapshot.
// That keeps the recommendation a real rollout brake: holding a bad release
// back stops every client from installing it.
// ---------------------------------------------------------------------------

// How long to let a graceful shutdown drain. `shutdown` stops recording
// immediately and then finishes post-processing, so this only bounds remuxing.
// We never escalate to shutdown_force here: cutting a remux short to save a
// couple of minutes would corrupt the file the user just agreed to keep.
const DRAIN_TIMEOUT_MS = 5 * 60_000;
// How long a freshly-spawned daemon gets to answer on NATS before we call it
// dead and roll back. Matches the first-run spawn budget in ensureConnected.
const READY_TIMEOUT_MS = 30_000;

/**
 * DaemonHost is the lifecycle surface DaemonUpdater needs from index.ts, which
 * owns the NATS session. Injected rather than imported so this module stays
 * free of a circular dependency on index.ts.
 */
export type DaemonHost = {
  /** Running daemon version from the live snapshot (daemon.version). */
  version(): string | null;
  /** Cloud recommended_version from the heartbeat, or null if not seen yet. */
  recommendedVersion(): string | null;
  /** True when the service refuses this daemon's version outright. */
  updateRequired(): boolean;
  /** Whether the daemon is Docker-managed (its binary can't be swapped). */
  container(): boolean;
  /**
   * Whether the app is an unpackaged dev build. scripts/build.sh stamps the
   * daemon with the latest git tag, so a locally built daemon reports a
   * perfectly plausible release version — the version alone can't tell us to
   * leave it alone. The app's own channel can.
   */
  devBuild(): boolean;
  /**
   * Whether the info endpoint has answered yet. The KV snapshot lands before it
   * does, so without this a container's daemon would briefly look host-managed
   * and we'd download a binary it can never run.
   */
  infoReady(): boolean;
  /** Commit the running daemon was built from (DaemonInfo.commit). */
  commit(): string | null;
  /** Binary the running daemon was launched from (DaemonInfo.executable_path). */
  executablePath(): string | null;
  /** Ask the daemon to stop; resolves once the request is away, not on exit. */
  shutdown(): Promise<void>;
  /** Resolves true when the daemon's NATS connection drops, false on timeout. */
  waitForExit(timeoutMs: number): Promise<boolean>;
  /** Tear the session down so it can connect afresh. */
  resetSession(): Promise<void>;
  /** Launch a daemon binary, detached. */
  spawn(exePath: string): void;
  /** Poll until the daemon answers and the session is started, or time out. */
  waitForReady(timeoutMs: number): Promise<boolean>;
};

// isReleaseVersion rejects "devel" and anything else that isn't a real release,
// so a locally built daemon is never auto-updated out from under a developer.
function isReleaseVersion(v: string): boolean {
  return /^v?\d+\.\d+/.test(v);
}

// pathOverride reports whether the user pinned a specific daemon binary; if so
// it's theirs to manage and we leave it alone.
function pathOverride(): boolean {
  return Boolean(process.env.VLREC_DAEMON_PATH || process.env.VLREC_DAEMON_BIN);
}

function emptyStatus(): DaemonUpdateStatus {
  return {
    phase: "idle",
    currentVersion: "",
    availableVersion: null,
    progress: null,
    message: null,
    container: false,
  };
}

export class DaemonUpdater {
  private status: DaemonUpdateStatus = emptyStatus();
  private onChange: () => void = () => {};
  // Coalesces overlapping checks (KV change + tab mount landing together).
  private checking: Promise<DaemonUpdateStatus> | null = null;
  // The downloaded binary waiting to be installed, and the version it holds.
  private readyPath: string | null = null;
  private readyVersion: string | null = null;

  constructor(private readonly host: DaemonHost) {}

  setOnChange(cb: () => void): void {
    this.onChange = cb;
  }

  getStatus(): DaemonUpdateStatus {
    return this.status;
  }

  isReady(): boolean {
    return this.status.phase === "ready" && this.readyPath !== null;
  }

  private set(patch: Partial<DaemonUpdateStatus>): void {
    this.status = { ...this.status, ...patch };
    try {
      this.onChange();
    } catch {
      /* transport not ready; the webview pulls state on mount */
    }
  }

  // check re-evaluates the running version against the cloud's recommendation
  // and downloads the new binary when one is due. Driven by the daemon.version /
  // daemon.upstream_version KV changes and by the Update tab on mount.
  check(): Promise<DaemonUpdateStatus> {
    if (this.checking) return this.checking;
    this.checking = this.runCheck().finally(() => {
      this.checking = null;
    });
    return this.checking;
  }

  private async runCheck(): Promise<DaemonUpdateStatus> {
    const current = this.host.version() ?? "";
    const container = this.host.container();
    this.set({ currentVersion: current, container });

    // Cases where the binary isn't ours to replace. Each is a normal state, not
    // an error — the tab explains it and shows no install button.
    if (container) {
      this.set({
        phase: "disabled",
        availableVersion: null,
        progress: null,
        message:
          "The recorder runs in Docker — update it with:\n  docker compose pull && docker compose up -d",
      });
      return this.status;
    }
    if (pathOverride()) {
      this.set({
        phase: "disabled",
        availableVersion: null,
        progress: null,
        message: "Running a daemon pinned by VLREC_DAEMON_PATH; updates are off.",
      });
      return this.status;
    }
    if (this.host.devBuild()) {
      // A developer's daemon, built and launched by hand. Downloading a release
      // binary over the top of it — and restarting into it — is the last thing
      // they want.
      this.set({
        phase: "disabled",
        availableVersion: null,
        progress: null,
        message: devBuildMessage(this.host.commit()?.slice(0, 7) ?? null),
      });
      return this.status;
    }
    if (!current || !this.host.infoReady()) {
      // Not connected yet, or the snapshot/info endpoint hasn't caught up.
      this.set({ phase: "idle", availableVersion: null, progress: null, message: null });
      return this.status;
    }
    if (!isReleaseVersion(current)) {
      this.set({
        phase: "disabled",
        availableVersion: null,
        progress: null,
        message: `Development build (${current}); updates are off.`,
      });
      return this.status;
    }

    let recommended = this.host.recommendedVersion();
    if (!recommended && this.host.updateRequired()) {
      // The service has stopped serving this version. It answers the heartbeat
      // for exactly this reason, but if that has not landed the daemon would
      // sit refused with no version to move to, so ask the releases instead.
      recommended = await latestDaemonVersion(binRepo()).catch(() => null);
    }
    if (!recommended) {
      // The heartbeat hasn't landed yet; it fires at daemon start, so this is
      // a brief window right after connecting.
      this.set({ phase: "checking", message: null });
      return this.status;
    }
    // compareVersionsDesc sorts newest-first, so < 0 means recommended is newer.
    if (compareVersionsDesc(recommended, current) >= 0) {
      this.readyPath = null;
      this.readyVersion = null;
      this.set({
        phase: "up-to-date",
        availableVersion: null,
        progress: null,
        message: null,
      });
      return this.status;
    }
    // Already downloaded this exact version — don't fetch it twice.
    if (this.readyVersion === recommended && this.readyPath) {
      this.set({ phase: "ready", availableVersion: recommended, progress: null });
      return this.status;
    }

    this.set({
      phase: "downloading",
      availableVersion: recommended,
      progress: null,
      message: null,
    });
    try {
      const path = await downloadDaemon(binRepo(), recommended, (pct) =>
        this.set({ progress: pct }),
      );
      this.readyPath = path;
      this.readyVersion = recommended;
      this.set({ phase: "ready", progress: null });
      void notifyOnce("daemon", recommended, {
        title: "Recorder update ready to install",
        body: `Recorder ${recommended} is ready. Open the app → Update to install it.`,
      });
    } catch (err) {
      this.readyPath = null;
      this.readyVersion = null;
      this.set({
        phase: "error",
        progress: null,
        message: err instanceof Error ? err.message : String(err),
      });
    }
    return this.status;
  }

  // launch starts a daemon binary and reports whether it came up serving.
  // spawn() itself can throw (a truncated or non-executable download is exactly
  // the case rollback exists for), and an uncaught throw here would strand the
  // install mid-sequence instead of recovering — so a failure to launch is just
  // another way of not becoming ready.
  private async launch(exePath: string): Promise<boolean> {
    try {
      this.host.spawn(exePath);
    } catch {
      return false;
    }
    return this.host.waitForReady(READY_TIMEOUT_MS);
  }

  /**
   * restart stops the running daemon and brings the downloaded version up in its
   * place. On failure it puts the previously-running binary back, so a bad
   * release leaves the user with a working recorder rather than none.
   *
   * Returns ok:false when the new version didn't come up — the caller must then
   * abandon the app update too, so the user is never left with a new app and a
   * broken recorder.
   */
  async restart(): Promise<{ ok: boolean; message: string | null }> {
    const newPath = this.readyPath;
    if (!newPath) return { ok: true, message: null };

    const target = this.status.availableVersion ?? "";
    const previous = this.status.currentVersion;
    // Captured before the shutdown: once the daemon is gone we can't ask it.
    const oldPath = this.host.executablePath();

    // Arm the exit waiter BEFORE asking the daemon to stop: an idle daemon can
    // drop its connection before the shutdown request's own promise resolves,
    // and a missed disconnect would leave us waiting out the whole drain window.
    const exited = this.host.waitForExit(DRAIN_TIMEOUT_MS);
    await this.host.shutdown();
    if (!(await exited)) {
      // Still draining post-processing. Reconnecting picks the daemon back up
      // where it is; forcing it down would cut a remux mid-file.
      await this.host.resetSession();
      await this.host.waitForReady(READY_TIMEOUT_MS);
      const message =
        "The recorder is still saving files. Try installing again in a few minutes.";
      this.set({ phase: "ready", message });
      return { ok: false, message };
    }

    await this.host.resetSession();
    if (await this.launch(newPath)) {
      this.readyPath = null;
      this.readyVersion = null;
      // The snapshot's daemon.version now reports the new build; re-check so the
      // tab settles on up-to-date.
      void this.check();
      return { ok: true, message: null };
    }

    // The new binary didn't come up. Put the old one back.
    let recovered = false;
    if (oldPath) {
      await this.host.resetSession();
      recovered = await this.launch(oldPath);
    }
    const message = recovered
      ? `Couldn't start version ${target}. Still running ${previous}.`
      : `Couldn't start version ${target}, and the previous version didn't come back. Restart the app to recover.`;
    this.readyPath = null;
    this.readyVersion = null;
    this.set({ phase: "error", progress: null, message });
    return { ok: false, message };
  }
}

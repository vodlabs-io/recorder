import { describe, expect, test } from "bun:test";
import type {
  EncodeQueue,
  PlatformLoginView,
  StorageRoot,
  StreamersSnapshot,
  StreamerView,
  UpdateState,
} from "../../../shared/types";
import {
  buildTrayMenu,
  trayActionRoute,
  TRAY_ADD_STREAMER,
  TRAY_INSTALL_UPDATE,
  TRAY_OPEN_RECORDINGS,
  TRAY_PAUSE_ENCODING,
  TRAY_RESUME_ENCODING,
  type TrayMenuInput,
} from "./tray-menu";

function streamer(over: Partial<StreamerView> = {}): StreamerView {
  return {
    platform: "twitch",
    username: "someone",
    liveStatus: "offline",
    monitor: true,
    record: true,
    variant: "",
    outputTemplate: "",
    deletePartFiles: null,
    splitSize: "",
    splitDuration: "",
    archiveDir: "",
    activity: "monitoring",
    stats: null,
    addedAt: null,
    lastLiveStart: null,
    lastLiveDurationSec: null,
    ...over,
  };
}

function recording(speed: number): StreamerView {
  return streamer({
    liveStatus: "live",
    activity: "recording",
    stats: { totalBytes: 1, durationMs: 1, speed },
  });
}

function login(platform: string, healthy: boolean): PlatformLoginView {
  return { platform, expiresHint: 0, capturedAt: 0, cookieCount: 1, healthy, lastCheckedAt: 0 };
}

function root(state: StorageRoot["state"], path: string): StorageRoot {
  return {
    paths: [path],
    download: true,
    usage: { total_bytes: 1 << 30, free_bytes: 1 << 20 },
    state,
  };
}

function snapshot(over: {
  streamers?: StreamerView[];
  accounts?: PlatformLoginView[];
  encode?: StreamersSnapshot["encode"];
  daemon?: Partial<StreamersSnapshot["daemon"]>;
} = {}): StreamersSnapshot {
  return {
    streamers: over.streamers ?? [],
    accounts: over.accounts ?? [],
    encode: over.encode ?? { queueRev: null, pending: 0, progress: null },
    daemon: {
      version: null,
      recommendedVersion: null,
      updateRequired: false,
      blocked: false,
      blockedUntil: null,
      service: null,
      startTime: null,
      subscriptionCount: 0,
      workerCount: 0,
      serverVersion: null,
      serverStartedAt: null,
      diskStatus: null,
      registration: null,
      ...over.daemon,
    },
  };
}

function update(
  over: {
    app?: string;
    appPhase?: UpdateState["app"]["phase"];
    daemon?: string;
    daemonPhase?: UpdateState["daemon"]["phase"];
    install?: UpdateState["install"]["phase"];
  } = {},
): UpdateState {
  return {
    app: {
      phase: over.appPhase ?? "idle",
      currentVersion: "1",
      channel: "stable",
      availableVersion: over.app ?? null,
      progress: null,
      message: null,
      build: null,
    },
    daemon: {
      phase: over.daemonPhase ?? "idle",
      currentVersion: "1",
      availableVersion: over.daemon ?? null,
      progress: null,
      message: null,
      container: false,
    },
    install: { phase: over.install ?? "idle", message: null },
  };
}

function menu(over: Partial<TrayMenuInput> = {}) {
  return buildTrayMenu({
    connection: { state: "connected" },
    snapshot: snapshot(),
    update: update(),
    encodeQueue: null,
    platformNames: new Map(),
    canOpenRecordings: true,
    liveFigures: true,
    ...over,
  });
}

function labels(over: Partial<TrayMenuInput> = {}): string[] {
  return menu(over).items.flatMap((i) => (i.type === "normal" ? [i.label] : []));
}

function actionOf(label: string, over: Partial<TrayMenuInput> = {}): string | undefined {
  const item = menu(over).items.find((i) => i.type === "normal" && i.label.includes(label));
  return item?.type === "normal" ? item.action : undefined;
}

function routeOf(label: string, over: Partial<TrayMenuInput> = {}): string | null {
  const item = menu(over).items.find((i) => i.type === "normal" && i.label.includes(label));
  return item?.type === "normal" ? trayActionRoute(item.action) : null;
}

describe("figures", () => {
  test("sums the download speed across recordings", () => {
    const streamers = [recording(1024 * 1024), recording(1024 * 1024 * 2)];
    expect(labels({ snapshot: snapshot({ streamers }) })).toContain("2 recording · 3.0 MB/s");
  });

  test("tells live streamers apart from the ones being recorded", () => {
    const streamers = [recording(1), streamer({ liveStatus: "live" }), streamer()];
    expect(labels({ snapshot: snapshot({ streamers }) })).toContain("2 live, 1 not recording");
  });

  // Neither monitored nor recorded means nothing checks it, so "live" is stale.
  test("doesn't count a live status nothing is checking", () => {
    const streamers = [streamer({ liveStatus: "live", monitor: false, record: false })];
    expect(labels({ snapshot: snapshot({ streamers }) })).toContain("No streamers live");
  });

  test("describes the encode queue", () => {
    const running = { job_id: 1, stage: "encode" as const, percent: 45.4 };
    const encode = (pending: number, progress: typeof running | null = null) =>
      snapshot({ encode: { queueRev: "1", pending, progress } });
    const paused: EncodeQueue = { jobs: [], paused: true };

    expect(labels()).toContain("Encoding queue empty");
    expect(labels({ encodeQueue: paused })).toContain("Encoding queue empty · paused");
    expect(labels({ snapshot: encode(3, running) })).toContain("Encoding 45% · 2 queued");
    expect(labels({ snapshot: encode(2) })).toContain("2 queued to encode");
    expect(labels({ snapshot: encode(2), encodeQueue: paused })).toContain("Encoding paused · 2 queued");
  });

  test("leaves fast-moving figures out where the menu can't take the rebuilds", () => {
    const running = { job_id: 1, stage: "encode" as const, percent: 45 };
    const got = labels({
      liveFigures: false,
      snapshot: snapshot({
        streamers: [recording(1024)],
        encode: { queueRev: "1", pending: 1, progress: running },
        daemon: {
          diskStatus: { roots: [root("low", "/a")], warn_bytes: 1, sampled_at: "" },
        },
      }),
    });
    expect(got).toContain("1 recording");
    expect(got).toContain("Encoding");
    expect(got).toContain("⚠ Low disk space: /a");
  });

  test("the tooltip keeps the speed either way", () => {
    const tip = menu({ liveFigures: false, snapshot: snapshot({ streamers: [recording(2048)] }) }).tooltip;
    expect(tip).toBe("VodLabs Recorder: 1 recording at 2.0 KB/s, 1 live");
  });
});

describe("attention", () => {
  test("a healthy idle app has nothing to flag", () => {
    expect(labels().filter((l) => l.startsWith("⚠"))).toEqual([]);
  });

  test("names expired logins and opens Accounts", () => {
    const over = {
      snapshot: snapshot({ accounts: [login("twitch", false), login("kick", true)] }),
      platformNames: new Map([["twitch", "Twitch"]]),
    };
    expect(labels(over)).toContain("⚠ 1 login expired: Twitch");
    expect(routeOf("login expired", over)).toBe("#/accounts");
  });

  test("caps the names of expired logins", () => {
    const accounts = ["a", "b", "c", "d", "e"].map((p) => login(p, false));
    expect(labels({ snapshot: snapshot({ accounts }) })).toContain("⚠ 5 logins expired: a, b, c +2 more");
  });

  test("reports each storage problem and opens Files", () => {
    const over = {
      snapshot: snapshot({
        daemon: {
          diskStatus: {
            roots: [root("low", "/low"), root("full", "/full"), root("unknown", "/nas"), root("ok", "/ok")],
            warn_bytes: 1,
            sampled_at: "",
          },
        },
      }),
    };
    const warnings = labels(over).filter((l) => l.startsWith("⚠"));
    expect(warnings).toEqual([
      "⚠ Out of space: /full",
      "⚠ Folder unavailable: /nas",
      "⚠ Low disk space: /low (1.0 MB free)",
    ]);
    expect(routeOf("Folder unavailable", over)).toBe("#/settings/files");
  });

  test("keeps the end of a long folder path", () => {
    const long = `/Volumes/${"x".repeat(60)}/recordings`;
    const got = labels({
      snapshot: snapshot({
        daemon: { diskStatus: { roots: [root("unknown", long)], warn_bytes: 1, sampled_at: "" } },
      }),
    });
    const row = got.find((l) => l.startsWith("⚠"))!;
    expect(row).toContain("…");
    expect(row.endsWith("/recordings")).toBe(true);
  });

  test("flags streamers that are losing a broadcast or need a sign-in", () => {
    const streamers = [
      streamer({ activity: "blocked_no_space", liveStatus: "live" }),
      streamer({ liveStatus: "auth_required" }),
      streamer({ activity: "moving_retrying" }),
    ];
    const got = labels({ snapshot: snapshot({ streamers }) });
    expect(got).toContain("⚠ 1 live streamer not recording: no space");
    expect(got).toContain("⚠ 1 streamer needs a sign-in");
    expect(got).toContain("⚠ 1 recording can't be moved to the archive");
  });

  test("flags failed encoding jobs and opens Encoding", () => {
    const job = { status: "failed" } as EncodeQueue["jobs"][number];
    const over = { encodeQueue: { jobs: [job, job], paused: false } };
    expect(labels(over)).toContain("⚠ 2 encoding jobs failed");
    expect(routeOf("encoding jobs failed", over)).toBe("#/encoding");
  });

  test("reports one service problem at a time, the most specific first", () => {
    const service = { reachable: false, since: "", updated_at: "" };
    expect(labels({ snapshot: snapshot({ daemon: { service } }) })).toContain("⚠ Can't reach the VodLabs service");
    const blocked = labels({ snapshot: snapshot({ daemon: { service, blocked: true } }) });
    expect(blocked).toContain("⚠ Blocked from the VodLabs service");
    expect(blocked).not.toContain("⚠ Can't reach the VodLabs service");
  });

  test("offers an update, and the install once it is ready", () => {
    expect(labels({ update: update({ app: "2.0.0" }) })).toContain("Update available: 2.0.0");
    expect(labels({ update: update({ daemon: "3.0.0" }) })).toContain("Update available: 3.0.0");
    expect(routeOf("Update available", { update: update({ app: "2.0.0" }) })).toBe("#/update");

    const ready = { update: update({ app: "2.0.0", appPhase: "ready" }) };
    expect(actionOf("Install Update and Restart: 2.0.0", ready)).toBe(TRAY_INSTALL_UPDATE);
  });

  // Same rule as the Update tab's button: a pair mid-download can't install twice.
  test("holds the install while the other half is still downloading", () => {
    const over = {
      update: update({ app: "2.0.0", appPhase: "ready", daemon: "2.0.0", daemonPhase: "downloading" }),
    };
    expect(labels(over)).toContain("Update available: 2.0.0");
    expect(labels(over).some((l) => l.startsWith("Install"))).toBe(false);
  });

  test("says when an install is running or failed", () => {
    const running = update({ app: "2.0.0", appPhase: "ready", install: "installing-app" });
    expect(labels({ update: running })).toContain("Installing update…");
    expect(labels({ update: update({ install: "error" }) })).toContain("⚠ Update didn't install");
  });

  test("counts what asks something of the user for the badge and tooltip", () => {
    const accounts = [login("a", false)];
    const expired = menu({ snapshot: snapshot({ accounts }) });
    expect(expired.attention).toBe(1);
    expect(expired.tooltip).toBe("VodLabs Recorder: not recording, 0 live, 1 needs attention");

    expect(menu({ update: update({ app: "2.0.0", appPhase: "ready" }) }).attention).toBe(1);
    // Still downloading: there is nothing to do yet.
    expect(menu({ update: update({ app: "2.0.0", appPhase: "downloading" }) }).attention).toBe(0);
    expect(menu().attention).toBe(0);
  });
});

describe("without a daemon", () => {
  test("says so, hides the figures and can't add a streamer", () => {
    const over: Partial<TrayMenuInput> = {
      connection: { state: "disconnected" },
      snapshot: snapshot({ streamers: [recording(1)], accounts: [login("a", false)] }),
    };
    const got = labels(over);
    expect(got[0]).toBe("⚠ Not connected to the recorder");
    expect(got.some((l) => l.includes("recording ·") || l.includes("login"))).toBe(false);
    const add = menu(over).items.find((i) => i.type === "normal" && i.action === TRAY_ADD_STREAMER);
    expect(add).toMatchObject({ enabled: false });
    const open = menu(over).items.find((i) => i.type === "normal" && i.action === TRAY_OPEN_RECORDINGS);
    expect(open).toMatchObject({ enabled: false });
  });

  // Starting up isn't a problem for the user to look at.
  test("only badges a real problem", () => {
    expect(menu({ connection: { state: "connecting" } }).attention).toBe(0);
    expect(menu({ connection: { state: "disconnected" } }).attention).toBe(1);
  });
});

describe("quick actions", () => {
  const encoding = (pending: number) => snapshot({ encode: { queueRev: "1", pending, progress: null } });

  test("pauses a queue with work in it", () => {
    expect(actionOf("Pause Encoding", { snapshot: encoding(2) })).toBe(TRAY_PAUSE_ENCODING);
    expect(labels().some((l) => l.includes("Pause Encoding"))).toBe(false);
  });

  // An empty paused queue would otherwise hold back the next job with no way out.
  test("can always resume a paused queue", () => {
    const encodeQueue: EncodeQueue = { jobs: [], paused: true };
    expect(actionOf("Resume Encoding", { encodeQueue })).toBe(TRAY_RESUME_ENCODING);
    expect(actionOf("Resume Encoding", { encodeQueue, snapshot: encoding(2) })).toBe(TRAY_RESUME_ENCODING);
    expect(labels({ encodeQueue, snapshot: encoding(2) })).not.toContain("Pause Encoding");
  });

  test("hides the recordings folder when its path isn't this machine's", () => {
    expect(actionOf("Open Recordings Folder")).toBe(TRAY_OPEN_RECORDINGS);
    expect(labels({ canOpenRecordings: false })).not.toContain("Open Recordings Folder");
  });
});

describe("layout", () => {
  test("attention first, then figures, then actions, then Quit", () => {
    const items = menu({ snapshot: snapshot({ accounts: [login("a", false)] }) }).items;
    const kinds = items.map((i) => (i.type === "separator" ? "-" : i.label));
    expect(kinds).toEqual([
      "⚠ 1 login expired: a",
      "-",
      "Not recording",
      "No streamers live",
      "Encoding queue empty",
      "-",
      "Open VodLabs Recorder",
      "Add Streamer…",
      "Open Recordings Folder",
      "-",
      "Quit VodLabs Recorder",
    ]);
  });
});

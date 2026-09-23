import { describe, expect, test } from "bun:test";
import { DaemonUpdater, type DaemonHost } from "./daemon-update";

// The restart sequence is the risky part of the daemon update: it stops a
// working daemon, and every failure branch has to leave the user with a running
// recorder and the app un-updated. These tests drive DaemonUpdater against a
// scripted host and assert the ordering and the recovery, since the real thing
// can only be exercised by actually taking the daemon down.

type HostScript = {
  // Whether waitForExit resolves (the daemon actually stopped).
  exits?: boolean;
  // waitForReady's answer per spawn, in order. Missing entries default to true.
  readyResults?: boolean[];
  executablePath?: string | null;
};

function makeHost(script: HostScript = {}) {
  const calls: string[] = [];
  const spawned: string[] = [];
  let readyIndex = 0;
  const host: DaemonHost = {
    version: () => "0.0.8",
    recommendedVersion: () => "0.0.9",
    updateRequired: () => false,
    container: () => false,
    devBuild: () => false,
    infoReady: () => true,
    executablePath: () =>
      script.executablePath === undefined ? "/old/vlrecd" : script.executablePath,
    shutdown: async () => {
      calls.push("shutdown");
    },
    waitForExit: async () => {
      calls.push("waitForExit");
      return script.exits ?? true;
    },
    resetSession: async () => {
      calls.push("reset");
    },
    spawn: (exePath) => {
      calls.push(`spawn:${exePath}`);
      spawned.push(exePath);
    },
    waitForReady: async () => {
      calls.push("waitForReady");
      return script.readyResults?.[readyIndex++] ?? true;
    },
  };
  return { host, calls, spawned };
}

// primeReady puts the updater in the "downloaded and waiting to install" state
// without touching the network, which is what restart() operates on.
function primeReady(updater: DaemonUpdater, path = "/cache/0.0.9/vlrecd"): void {
  const u = updater as unknown as {
    readyPath: string | null;
    readyVersion: string | null;
    status: { phase: string; currentVersion: string; availableVersion: string };
  };
  u.readyPath = path;
  u.readyVersion = "0.0.9";
  u.status = {
    ...u.status,
    phase: "ready",
    currentVersion: "0.0.8",
    availableVersion: "0.0.9",
  };
}

describe("DaemonUpdater.restart", () => {
  test("stops the daemon, then starts the downloaded binary", async () => {
    const { host, calls, spawned } = makeHost();
    const updater = new DaemonUpdater(host);
    primeReady(updater);

    const res = await updater.restart();

    expect(res.ok).toBe(true);
    expect(spawned).toEqual(["/cache/0.0.9/vlrecd"]);
    // The old daemon must be gone before the new binary is launched, or the new
    // one fails to bind the NATS port.
    expect(calls.indexOf("waitForExit")).toBeLessThan(
      calls.indexOf("spawn:/cache/0.0.9/vlrecd"),
    );
    expect(calls).toEqual([
      "waitForExit",
      "shutdown",
      "reset",
      "spawn:/cache/0.0.9/vlrecd",
      "waitForReady",
    ]);
  });

  test("arms the exit waiter before asking the daemon to stop", async () => {
    // An idle daemon can drop its connection before the shutdown request's own
    // promise resolves; waiting only afterwards would miss the disconnect.
    const { host, calls } = makeHost();
    const updater = new DaemonUpdater(host);
    primeReady(updater);

    await updater.restart();

    expect(calls.indexOf("waitForExit")).toBeLessThan(calls.indexOf("shutdown"));
  });

  test("rolls back to the previous binary when the new one won't start", async () => {
    const { host, calls, spawned } = makeHost({ readyResults: [false, true] });
    const updater = new DaemonUpdater(host);
    primeReady(updater);

    const res = await updater.restart();

    expect(res.ok).toBe(false);
    expect(spawned).toEqual(["/cache/0.0.9/vlrecd", "/old/vlrecd"]);
    expect(res.message).toContain("0.0.9");
    expect(res.message).toContain("Still running 0.0.8");
    expect(calls.at(-1)).toBe("waitForReady");
    // A failed install must not leave the download primed, or the next click
    // would retry the same broken binary.
    expect(updater.isReady()).toBe(false);
  });

  test("rolls back when spawning the new binary throws outright", async () => {
    // A truncated or non-executable download can fail at spawn rather than at
    // readiness; an uncaught throw here would strand the install mid-sequence.
    const { host, spawned } = makeHost();
    const updater = new DaemonUpdater({
      ...host,
      spawn: (p) => {
        if (p.startsWith("/cache")) throw new Error("EACCES");
        spawned.push(p);
      },
    });
    primeReady(updater);

    const res = await updater.restart();

    expect(res.ok).toBe(false);
    expect(spawned).toEqual(["/old/vlrecd"]);
    expect(res.message).toContain("Still running 0.0.8");
  });

  test("says so when even the previous binary won't come back", async () => {
    const { host } = makeHost({ readyResults: [false, false] });
    const updater = new DaemonUpdater(host);
    primeReady(updater);

    const res = await updater.restart();

    expect(res.ok).toBe(false);
    expect(res.message).toContain("didn't come back");
  });

  test("never force-kills a daemon that is still draining", async () => {
    // shutdown stops recording immediately and then finishes post-processing.
    // Cutting a remux short to save time would corrupt the file the user just
    // agreed to keep, so we reconnect and tell them to try again.
    const { host, spawned } = makeHost({ exits: false });
    const updater = new DaemonUpdater(host);
    primeReady(updater);

    const res = await updater.restart();

    expect(res.ok).toBe(false);
    expect(spawned).toEqual([]);
    expect(res.message).toContain("still saving files");
    // The download survives — nothing was installed, so the retry is free.
    expect(updater.isReady()).toBe(true);
  });

  test("is a no-op success when nothing was downloaded", async () => {
    const { host, calls } = makeHost();
    const updater = new DaemonUpdater(host);

    const res = await updater.restart();

    expect(res.ok).toBe(true);
    expect(calls).toEqual([]);
  });
});

describe("DaemonUpdater.check", () => {
  async function phaseFor(overrides: Partial<DaemonHost>) {
    const { host } = makeHost();
    const updater = new DaemonUpdater({ ...host, ...overrides });
    return (await updater.check()).phase;
  }

  test("leaves a containerized daemon alone", async () => {
    expect(await phaseFor({ container: () => true })).toBe("disabled");
  });

  test("leaves a dev build alone", async () => {
    // scripts/build.sh stamps the latest git tag, so a locally built daemon
    // reports a plausible release version — only the app's channel can tell.
    expect(await phaseFor({ devBuild: () => true })).toBe("disabled");
  });

  test("waits for the info endpoint before judging", async () => {
    expect(await phaseFor({ infoReady: () => false })).toBe("idle");
  });

  test("does nothing when already on the recommended version", async () => {
    expect(await phaseFor({ version: () => "0.0.9" })).toBe("up-to-date");
  });

  test("does not downgrade a version ahead of the recommendation", async () => {
    expect(await phaseFor({ version: () => "0.1.0" })).toBe("up-to-date");
  });

  test("waits for the heartbeat before reporting", async () => {
    expect(await phaseFor({ recommendedVersion: () => null })).toBe("checking");
  });

  // A daemon the service has stopped serving cannot learn a version from
  // anything but the heartbeat, so a heartbeat that never lands would leave it
  // refused forever. The releases are the other answer.
  test("asks the releases when the service refuses this version", async () => {
    const prev = process.env.VLREC_BIN_REPO;
    process.env.VLREC_BIN_REPO = "https://example.invalid/no/such/repo";
    try {
      expect(
        await phaseFor({ recommendedVersion: () => null, updateRequired: () => true }),
      ).toBe("checking"); // the lookup fails here, leaving it where it was
    } finally {
      if (prev === undefined) delete process.env.VLREC_BIN_REPO;
      else process.env.VLREC_BIN_REPO = prev;
    }
  });
});

describe("re-checking is idempotent", () => {
  test("a ready daemon download is not fetched twice", async () => {
    // The Update tab re-checks on every mount; a ready half must stay put
    // rather than replaying its download and flickering the row.
    const { host } = makeHost();
    const updater = new DaemonUpdater(host);
    primeReady(updater);

    const phases: string[] = [];
    updater.setOnChange(() => phases.push(updater.getStatus().phase));
    const status = await updater.check();

    expect(status.phase).toBe("ready");
    expect(phases).not.toContain("downloading");
    expect(phases).not.toContain("checking");
  });
});

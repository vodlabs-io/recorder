import { describe, expect, test } from "bun:test";
import type { StreamerView } from "@/lib/daemon";
import { statusKey, streamerIndicator } from "./status";

function view(overrides: Partial<StreamerView>): StreamerView {
  return {
    platform: "tiktok",
    username: "someone",
    liveStatus: "live",
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
    ...overrides,
  };
}

describe("login wall during a recording", () => {
  // A room recorded through a stored login still reads as walled to anyone
  // without one, and that status can land while the recording runs.
  test("an active session outranks restricted", () => {
    for (const activity of ["recording", "lagging", "paused"] as const) {
      const s = view({ liveStatus: "restricted", activity });
      expect(statusKey(s)).not.toBe("restricted");
      expect(streamerIndicator(s).tooltip).not.toBe("Restricted");
    }
    const s = view({ liveStatus: "restricted", activity: "recording" });
    expect(statusKey(s)).toBe("recording");
    expect(streamerIndicator(s)).toEqual({ state: "ok", tooltip: "Recording" });
  });

  test("restricted still shows when nothing is recording", () => {
    for (const activity of ["monitoring", "idle", "processing", "moving"] as const) {
      const s = view({ liveStatus: "restricted", activity });
      expect(statusKey(s)).toBe("restricted");
      expect(streamerIndicator(s).tooltip).toBe("Restricted");
    }
  });
});

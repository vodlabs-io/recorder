import { describe, expect, test } from "bun:test";
import { shouldNotifyKvChange } from "./session";
import { VODS_REV_KEY } from "./subjects";

describe("shouldNotifyKvChange", () => {
  test("dispatches entries whose state actually changed", () => {
    expect(shouldNotifyKvChange("streamer.tiktok.alice.state", true)).toBe(true);
    expect(shouldNotifyKvChange(VODS_REV_KEY, true)).toBe(true);
  });

  test("swallows unchanged rewrites of ordinary keys", () => {
    expect(shouldNotifyKvChange("daemon.heartbeat", false)).toBe(false);
    expect(shouldNotifyKvChange("streamer.tiktok.alice.state", false)).toBe(false);
  });

  // The regression this exists for: a scan that finds nothing re-writes the
  // same rev, and that write is the only signal the walk has finished. Dropping
  // it leaves the VOD Archive showing "Scanning for VODs…" indefinitely.
  test("dispatches an unchanged rewrite of the VOD Archive rev", () => {
    expect(shouldNotifyKvChange(VODS_REV_KEY, false)).toBe(true);
  });
});

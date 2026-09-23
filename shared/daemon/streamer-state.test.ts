import { describe, expect, test } from "bun:test";
import { StreamerState } from "./streamer-state";

const aliceDoc = JSON.stringify({
  schema: 1,
  platform: "tiktok",
  username: "alice",
  status: "live",
  added_at: 1690000000,
  last_live_start: 1700000000,
  monitor: true,
  record: true,
  variant: "720p",
  activity: "recording",
  active_session_id: "sess1",
});

const aliceStats = JSON.stringify({
  total_bytes: 1024,
  duration_ns: 2_500_000_000,
  speed_bps: 512,
  session_id: "sess1",
});

describe("StreamerState.snapshot", () => {
  test("maps docs to views and attaches stats via active_session_id", () => {
    const s = new StreamerState();
    s.applyKvEntry("streamer.tiktok.alice.state", "PUT", aliceDoc);
    s.applyKvEntry(
      "streamer.tiktok.alice.session.sess1.stats",
      "PUT",
      aliceStats,
    );
    // A foreign session's stats and non-doc keys must not leak in.
    s.applyKvEntry("streamer.tiktok.alice.session.old.stats", "PUT", aliceStats);
    s.applyKvEntry("streamer.tiktok.alice.monitor", "PUT", "false");

    const snap = s.snapshot();
    expect(snap.streamers).toHaveLength(1);
    const v = snap.streamers[0]!;
    expect(v.username).toBe("alice");
    expect(v.liveStatus).toBe("live");
    expect(v.activity).toBe("recording");
    expect(v.variant).toBe("720p");
    expect(v.addedAt).toBe(1690000000);
    expect(v.lastLiveStart).toBe(1700000000);
    expect(v.stats).toEqual({ totalBytes: 1024, durationMs: 2500, speed: 512 });
  });

  test("row exists iff its doc exists", () => {
    const s = new StreamerState();
    // Residual session keys alone must not synthesize a row.
    s.applyKvEntry("streamer.tiktok.bob.session.s9.stats", "PUT", aliceStats);
    expect(s.snapshot().streamers).toHaveLength(0);

    s.applyKvEntry("streamer.url.abc123.state", "PUT", aliceDoc);
    expect(s.snapshot().streamers).toHaveLength(1);

    s.applyKvEntry("streamer.url.abc123.state", "DEL");
    expect(s.snapshot().streamers).toHaveLength(0);
  });

  test("collects daemon meta", () => {
    const s = new StreamerState();
    s.applyKvEntry("daemon.version", "PUT", "1.2.3");
    s.applyKvEntry("daemon.subscription_count", "PUT", "4");
    const snap = s.snapshot();
    expect(snap.daemon.version).toBe("1.2.3");
    expect(snap.daemon.subscriptionCount).toBe(4);
  });

  test("maps split fields from the doc", () => {
    const s = new StreamerState();
    const doc = JSON.stringify({
      schema: 1,
      platform: "tiktok",
      username: "carol",
      status: "offline",
      monitor: true,
      record: true,
      split_size: "2GB",
      split_duration: "1h30m",
    });
    s.applyKvEntry("streamer.tiktok.carol.state", "PUT", doc);
    const v = s.snapshot().streamers[0]!;
    expect(v.splitSize).toBe("2GB");
    expect(v.splitDuration).toBe("1h30m");
  });

  test("absent split fields default to empty string", () => {
    const s = new StreamerState();
    // aliceDoc carries no split_size / split_duration keys.
    s.applyKvEntry("streamer.tiktok.alice.state", "PUT", aliceDoc);
    const v = s.snapshot().streamers[0]!;
    expect(v.splitSize).toBe("");
    expect(v.splitDuration).toBe("");
  });

  test("maps avatar_url to avatarUrl", () => {
    const s = new StreamerState();
    const doc = JSON.stringify({
      schema: 1,
      platform: "twitch",
      username: "dave",
      status: "offline",
      monitor: true,
      record: true,
      avatar_url: "https://img.recordthis.live/streamer-avatars/twitch/x.jpg",
    });
    s.applyKvEntry("streamer.twitch.dave.state", "PUT", doc);
    const v = s.snapshot().streamers[0]!;
    expect(v.avatarUrl).toBe(
      "https://img.recordthis.live/streamer-avatars/twitch/x.jpg",
    );
  });

  test("absent avatar_url leaves avatarUrl undefined", () => {
    const s = new StreamerState();
    // aliceDoc carries no avatar_url key.
    s.applyKvEntry("streamer.tiktok.alice.state", "PUT", aliceDoc);
    const v = s.snapshot().streamers[0]!;
    expect(v.avatarUrl).toBeUndefined();
  });
});

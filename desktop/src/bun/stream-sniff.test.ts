import { describe, expect, test } from "bun:test";
import { isLiveEnough, rank, type Hit } from "./stream-sniff-rank";
import { NUDGE, SNIFFER } from "./stream-sniff-script";

const page = "https://www.news.example/live";

describe("rank", () => {
  // Euronews: the stream is fetched by a Dailymotion iframe, whose address is
  // the Referer the origin expects.
  test("credits the frame that fetched the stream", () => {
    const frame = "https://geo.dailymotion.com/player/x1.html?video=x2";
    const hits: Hit[] = [
      { kind: "resource", url: "https://www.dailymotion.com/cdn/live/video/x2.m3u8?sec=a", frame },
      { kind: "media", url: "https://www.dailymotion.com/cdn/live/video/x2.m3u8?sec=a", frame },
    ];
    expect(rank(hits)).toEqual([
      { url: "https://www.dailymotion.com/cdn/live/video/x2.m3u8?sec=a", format: "hls", frame },
    ]);
  });

  // Bloomberg / CBS News: ad SDK playlists come first in time; server-side ad
  // insertion is the content stream and stays.
  test("drops ad SDK hosts and keeps server-side ad insertion", () => {
    const hits: Hit[] = [
      { kind: "fetch", url: "https://imasdk.googleapis.com/js/core/x.m3u8", frame: page },
      { kind: "xhr", url: "https://dai.google.com/linear/hls/pa/event/abc/stream/def/master.m3u8", frame: page },
    ];
    expect(rank(hits).map((c) => c.url)).toEqual([
      "https://dai.google.com/linear/hls/pa/event/abc/stream/def/master.m3u8",
    ]);
  });

  test("prefers what is playing, then a re-read playlist, then a master", () => {
    const hits: Hit[] = [
      { kind: "html", url: "https://cdn.example/old/vod.m3u8", frame: page },
      { kind: "response", url: "https://cdn.example/live/master.m3u8", frame: page, master: true },
      { kind: "fetch", url: "https://cdn.example/live/master.m3u8", frame: page },
      { kind: "fetch", url: "https://cdn.example/live/720/index.m3u8", frame: page },
      { kind: "fetch", url: "https://cdn.example/live/720/index.m3u8", frame: page },
      { kind: "playing", url: "https://cdn.example/native/index.m3u8", frame: page },
    ];
    expect(rank(hits).map((c) => c.url)).toEqual([
      "https://cdn.example/native/index.m3u8",
      "https://cdn.example/live/720/index.m3u8",
      "https://cdn.example/live/master.m3u8",
      "https://cdn.example/old/vod.m3u8",
    ]);
  });

  test("drops ended playlists, blobs and anything that is not HLS or FLV", () => {
    const hits: Hit[] = [
      { kind: "response", url: "https://cdn.example/vod/index.m3u8", frame: page, ended: true },
      { kind: "fetch", url: "https://cdn.example/vod/index.m3u8", frame: page },
      { kind: "media", url: "blob:https://www.news.example/123", frame: page },
      { kind: "fetch", url: "https://cdn.example/dash/manifest.mpd", frame: page },
      { kind: "fetch", url: "https://pull.example/live/room.flv?txSecret=1", frame: page },
    ];
    expect(rank(hits)).toEqual([
      { url: "https://pull.example/live/room.flv?txSecret=1", format: "flv", frame: page },
    ]);
  });
});

// Euronews' Dailymotion player fetches its ad schedule from a .m3u8 address
// that answers with VMAP XML.
test("drops a playlist address that answered with something else", () => {
  const ads = "https://dmxleo.dailymotion.com/cdn/manifest/video/x.m3u8?af=2";
  const hits: Hit[] = [
    { kind: "fetch", url: ads, frame: page },
    { kind: "fetch", url: ads, frame: page },
    { kind: "response", url: ads, frame: page, invalid: true },
  ];
  expect(rank(hits)).toEqual([]);
  expect(isLiveEnough(hits)).toBe(false);
});

// 3Cat: a Youbora analytics beacon carries the stream's address in its query.
test("ranks a playlist named only in a query below a real one", () => {
  const beacon = "https://c40.youboranqs01.com/start?accountCode=tv3&mediaResource=https%3A%2F%2Fx%2Fmaster.m3u8";
  const real = "https://directes3-tv-int.3catdirectes.cat/live-content/tvi-hls/master.m3u8";
  const hits: Hit[] = [
    { kind: "resource", url: beacon, frame: page },
    { kind: "html", url: real, frame: page },
  ];
  expect(rank(hits).map((c) => c.url)).toEqual([real, beacon]);
});

describe("isLiveEnough", () => {
  test("waits for a sign the stream is live", () => {
    const once: Hit = { kind: "fetch", url: "https://cdn.example/live/index.m3u8", frame: page };
    const read: Hit = { kind: "response", url: once.url, frame: page };
    expect(isLiveEnough([once])).toBe(false);
    expect(isLiveEnough([once, once])).toBe(false);
    expect(isLiveEnough([once, read, once])).toBe(true);
    expect(isLiveEnough([{ kind: "playing", url: once.url, frame: page }])).toBe(true);
    expect(isLiveEnough([{ kind: "fetch", url: "https://pull.example/a.flv", frame: page }])).toBe(true);
  });
});

describe("scripts", () => {
  test("parse", () => {
    expect(() => new Function(SNIFFER)).not.toThrow();
    expect(() => new Function(NUDGE)).not.toThrow();
    expect(SNIFFER.startsWith("/* electrobun:all-frames */")).toBe(true);
  });
});

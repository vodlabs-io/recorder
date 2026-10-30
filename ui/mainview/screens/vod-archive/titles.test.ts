import { describe, expect, test } from "bun:test"
import type { VOD } from "@/lib/daemon"
import { playerTitles, streamTitle } from "./format"

const vod = (over: Partial<VOD>): VOD =>
  ({
    dir: "/r/tiktok/someone/2026-09-27",
    platform: "tiktok",
    username: "someone",
    start: "2026-09-27T13:56:24Z",
    files: [
      { name: "vod1.mp4", bytes: 1, start: "2026-09-27T13:56:24Z", has_thumb: true },
      { name: "vod2.mp4", bytes: 1, start: "2026-09-27T14:30:00Z", has_thumb: true },
    ],
    ...over,
  }) as VOD

describe("streamTitle", () => {
  test("is the title in force when the file began", () => {
    const s = vod({
      titles: [
        { text: "morning drive", at: "2026-09-27T13:56:30Z" },
        { text: "lunch stop", at: "2026-09-27T14:20:00Z" },
      ],
    })
    expect(streamTitle(s, "vod1.mp4")).toBe("morning drive")
    expect(streamTitle(s, "vod2.mp4")).toBe("lunch stop")
  })
  test("is undefined without titles", () => {
    expect(streamTitle(vod({ titles: [{ text: "  " }] }), "vod1.mp4")).toBeUndefined()
  })
})

describe("playerTitles", () => {
  test("puts who and when under the stream title", () => {
    const t = playerTitles(vod({ titles: [{ text: "morning drive" }] }), "vod1.mp4")
    expect(t.title).toBe("morning drive")
    expect(t.subtitle).toStartWith("someone · TikTok · ")
  })
  test("falls back to who and when", () => {
    const t = playerTitles(vod({ display_name: "Some One" }), "vod1.mp4")
    expect(t.title).toStartWith("Some One · TikTok · ")
    expect(t.subtitle).toBeUndefined()
  })
})

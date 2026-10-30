import { describe, expect, test } from "bun:test"
import { frameAt, locate, panelAt, panelBox, spriteStyle } from "./scrub"

const previews = { duration_ms: 48_000, scrub: { cols: 8, count: 48, times_ms: [] } }

describe("locate", () => {
  const files = [
    { name: "vod1.mp4", durationMs: 60_000 },
    { name: "vod2.mp4", durationMs: 30_000 },
  ]
  test("maps the card's width across files by duration", () => {
    expect(locate(files, 0)).toEqual({ index: 0, ms: 0 })
    expect(locate(files, 0.5)).toEqual({ index: 0, ms: 45_000 })
    expect(locate(files, 2 / 3)).toEqual({ index: 1, ms: 0 })
    expect(locate(files, 1)).toEqual({ index: 1, ms: 30_000 })
  })
  test("clamps outside the card and handles nothing to show", () => {
    expect(locate(files, -1)).toEqual({ index: 0, ms: 0 })
    expect(locate([], 0.5)).toBeNull()
    expect(locate([{ name: "a", durationMs: 0 }], 0.5)).toBeNull()
  })
})

describe("frameAt", () => {
  test("picks the tile for each equal share", () => {
    expect(frameAt(previews, 0)).toBe(0)
    expect(frameAt(previews, 999)).toBe(0)
    expect(frameAt(previews, 1_000)).toBe(1)
    expect(frameAt(previews, 47_999)).toBe(47)
    expect(frameAt(previews, 60_000)).toBe(47)
  })
})

describe("triptych", () => {
  test("each third of the card is its own panel", () => {
    expect(panelAt(0)).toBe(0)
    expect(panelAt(0.34)).toBe(1)
    expect(panelAt(0.99)).toBe(2)
    expect(panelAt(1)).toBe(2)
  })
  test("panels follow the daemon's 480x270 layout", () => {
    expect(panelBox(0).left).toBe(0)
    expect(panelBox(2).left + panelBox(2).width).toBeCloseTo(((2 * 161 + 158) / 480) * 100)
  })
})

describe("spriteStyle", () => {
  test("crops a tile by percentage", () => {
    const s = spriteStyle(previews, 9, "data:x")
    expect(s.backgroundSize).toBe("800% 600%")
    expect(s.backgroundPosition).toBe(`${(1 / 7) * 100}% ${(1 / 5) * 100}%`)
  })
})

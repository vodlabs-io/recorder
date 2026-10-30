import { describe, expect, test } from "bun:test"
import { absolutizeCues } from "./thumbnails-track"

describe("absolutizeCues", () => {
  test("resolves cue images against the track, keeping the crop", () => {
    const vtt =
      "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n../preview?dir=%2Fa&file=vod1.mp4&kind=sheet&n=0#xywh=0,0,256,144\n"
    const out = absolutizeCues(vtt, "http://host/ui/api/vods/hls/thumbs.vtt?dir=%2Fa")
    expect(out).toContain("\nhttp://host/ui/api/vods/preview?dir=%2Fa&file=vod1.mp4&kind=sheet&n=0#xywh=0,0,256,144\n")
    expect(out).toContain("00:00:00.000 --> 00:00:01.000")
  })
})

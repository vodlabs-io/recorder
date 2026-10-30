import { useEffect, useState } from "react"

// absolutizeCues rewrites a thumbnails track's cue images against the
// track's own URL. Players resolve a relative cue image against the track
// only when they can find its <track> element, which the hls.js media isn't
// one of; absolute URLs need no resolving.
export function absolutizeCues(vtt: string, trackUrl: string): string {
  return vtt.replace(/^(?!WEBVTT)(?!\d\d:)([^\s#][^\s]*?)(#xywh=[\d,]+)$/gm, (_, img: string, frag: string) => {
    return new URL(img, trackUrl).href + frag
  })
}

// useThumbnailsTrack loads a timeline-previews track as a same-origin blob
// URL with absolute cue images. A missing or empty track yields undefined.
export function useThumbnailsTrack(url: string | undefined): string | undefined {
  const [blobUrl, setBlobUrl] = useState<string>()
  useEffect(() => {
    if (!url) return
    let cancelled = false
    let current: string | undefined
    void fetch(url, { cache: "no-store" })
      .then((res) => (res.ok ? res.text() : null))
      .catch(() => null)
      .then((text) => {
        if (cancelled || !text || !text.includes("-->")) return
        current = URL.createObjectURL(new Blob([absolutizeCues(text, url)], { type: "text/vtt" }))
        setBlobUrl(current)
      })
    return () => {
      cancelled = true
      setBlobUrl(undefined)
      if (current) URL.revokeObjectURL(current)
    }
  }, [url])
  return blobUrl
}

import { useCallback, useRef, useState } from "react"
import { getDaemonClient, type VODFile, type VODPreviews } from "@/lib/daemon"
import { coverBox, frameAt, locate, panelAt, panelBox, spriteStyle, type ScrubFile } from "./scrub"

type Loaded = { previews: VODPreviews; url: string }

// Hover strips are ~50-150 KB each; keep the recently hovered ones.
const STRIP_CACHE_MAX = 40
// A failed load (a busy VOD, an older daemon) is retried after this long
// rather than on every hover.
const FAILED_RETRY_MS = 60_000

const strips = new Map<string, { at: number; p: Promise<Loaded | null> }>()

// loadStrip fetches one file's preview layout and hover strip once; the
// daemon generates them on first request.
export function loadStrip(dir: string, file: string): Promise<Loaded | null> {
  const key = `${dir}|${file}`
  const hit = strips.get(key)
  if (hit) {
    strips.delete(key)
    strips.set(key, hit)
    return hit.p
  }
  const p = getDaemonClient()
    .getVodPreviews(dir, file)
    .then((previews) => ({ previews, url: `data:image/jpeg;base64,${previews.scrub_b64}` }))
    .catch(() => {
      setTimeout(() => strips.delete(key), FAILED_RETRY_MS)
      return null
    })
  strips.set(key, { at: Date.now(), p })
  for (const k of strips.keys()) {
    if (strips.size <= STRIP_CACHE_MAX) break
    strips.delete(k)
  }
  return p
}

export function scrubFiles(files: VODFile[], vodDurationSec: number): ScrubFile[] {
  const known = files.every((f) => (f.duration_sec ?? 0) > 0)
  return files.map((f) => ({
    name: f.name,
    durationMs: known ? (f.duration_sec ?? 0) * 1000 : (vodDurationSec * 1000) / Math.max(1, files.length),
  }))
}

// ScrubOverlay sits over a card's thumbnail. Moving the cursor across it
// shows the VOD from start (left) to end (right); over a three-up portrait
// thumbnail only the panel under the cursor changes, through its third of the
// VOD. Nothing is fetched until the first hover.
export function ScrubOverlay({ dir, files }: { dir: string; files: ScrubFile[] }) {
  const ref = useRef<HTMLDivElement | null>(null)
  const [fraction, setFraction] = useState<number | null>(null)
  const [loaded, setLoaded] = useState<Record<string, Loaded | null>>({})

  const ensure = useCallback(
    (name: string) => {
      if (name in loaded) return
      setLoaded((l) => ({ ...l, [name]: null }))
      void loadStrip(dir, name).then((s) => setLoaded((l) => ({ ...l, [name]: s })))
    },
    [dir, loaded],
  )

  // A file the listing has no duration for takes its previews' own.
  const timeline = files.map((f) =>
    f.durationMs > 0 ? f : { ...f, durationMs: loaded[f.name]?.previews.duration_ms ?? 0 },
  )

  const onMove = (e: React.PointerEvent) => {
    if (e.pointerType === "touch") return
    const r = ref.current?.getBoundingClientRect()
    if (!r || r.width <= 0) return
    const f = (e.clientX - r.left) / r.width
    setFraction(f)
    if (files[0]) ensure(files[0].name)
    const at = locate(timeline, f)
    if (at) ensure(files[at.index].name)
  }

  const first = files[0] ? loaded[files[0].name] : null
  const at = fraction == null ? null : locate(timeline, fraction)
  const strip = at ? loaded[files[at.index].name] : null
  let frame: React.ReactNode = null
  if (fraction != null && at && strip && first) {
    const style = spriteStyle(strip.previews, frameAt(strip.previews, at.ms), strip.url)
    if (first.previews.portrait) {
      const box = panelBox(panelAt(fraction))
      const cover = coverBox(...strip.previews.tile)
      frame = (
        <div
          className="absolute inset-y-0 overflow-hidden bg-black"
          style={{ left: `${box.left}%`, width: `${box.width}%` }}
        >
          <div
            className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 bg-no-repeat"
            style={{ ...style, width: `${cover.width}%`, height: `${cover.height}%` }}
          />
        </div>
      )
    } else {
      frame = <div className="absolute inset-0 bg-no-repeat" style={style} />
    }
  }

  return (
    <div
      ref={ref}
      className="absolute inset-0"
      onPointerMove={onMove}
      onPointerEnter={onMove}
      onPointerLeave={() => setFraction(null)}
    >
      {frame}
      {fraction != null && frame && (
        <div className="absolute inset-x-0 bottom-0 h-0.5 bg-black/40">
          <div className="h-full bg-primary" style={{ width: `${Math.min(1, Math.max(0, fraction)) * 100}%` }} />
        </div>
      )}
    </div>
  )
}

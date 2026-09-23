import { useEffect, useRef, useState, useSyncExternalStore } from "react"
import { Film, Loader2 } from "lucide-react"
import { getDaemonClient } from "@/lib/daemon"

// Thumbnails without a sidecar are generated on demand by the daemon
// (ffmpeg), which is slow. Firing one request per card during a fast scroll
// floods the daemon and pushes later requests past the NATS request timeout,
// so they fail and never load. Cap how many run at once so bursts queue on
// the client (each request's timeout only starts once it's actually sent)
// instead of failing.
const THUMB_CONCURRENCY = 4
// Stop retrying a thumbnail after this many failures so a genuinely broken or
// missing file doesn't re-request every time it scrolls back into view.
const MAX_THUMB_ATTEMPTS = 3
// Delay before an on-screen card automatically retries a failed load.
const RETRY_DELAY_MS = 1500

// Loaded data URLs are ~10-50 KB of base64 each; keeping every one of a large
// archive alive would grow into hundreds of MB as the user scrolls. Cap the
// cache LRU-style — evicted thumbnails re-fetch cheaply on scroll-back (the
// daemon keeps the generated JPEG on disk next to the video).
const THUMB_CACHE_MAX = 200

type ThumbEntry = {
  url?: string
  attempts: number
  inflight?: Promise<string | null>
}

// Module-level so the cache survives screen unmounts (tab switches).
const thumbCache = new Map<string, ThumbEntry>()

let thumbActive = 0
const thumbQueue: Array<() => void> = []

// runThumb runs task once a concurrency slot is free, releasing it (and
// starting the next queued task) when task settles.
function runThumb<T>(task: () => Promise<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const start = () => {
      thumbActive++
      task()
        .then(resolve, reject)
        .finally(() => {
          thumbActive--
          thumbQueue.shift()?.()
        })
    }
    if (thumbActive < THUMB_CONCURRENCY) start()
    else thumbQueue.push(start)
  })
}

// touch marks key as most-recently-used and prunes the oldest entries past the
// cap (Map preserves insertion order). Entries with a request in flight are
// never evicted mid-flight — they're the most recent by definition.
function touch(key: string, entry: ThumbEntry): void {
  thumbCache.delete(key)
  thumbCache.set(key, entry)
  for (const k of thumbCache.keys()) {
    if (thumbCache.size <= THUMB_CACHE_MAX) break
    thumbCache.delete(k)
  }
}

// loadThumb resolves the session file's thumbnail data URL, or null when it
// failed (retryable until MAX_THUMB_ATTEMPTS) or is exhausted. Concurrent
// callers for the same key share one request.
function loadThumb(dir: string, file: string): Promise<string | null> {
  const key = `${dir}|${file}`
  const entry = thumbCache.get(key) ?? { attempts: 0 }
  if (entry.url) {
    touch(key, entry)
    return Promise.resolve(entry.url)
  }
  if (entry.inflight) return entry.inflight
  if (entry.attempts >= MAX_THUMB_ATTEMPTS) return Promise.resolve(null)

  entry.inflight = runThumb(() => getDaemonClient().getRecordingThumbnail(dir, file))
    .then((b64) => {
      entry.url = `data:image/jpeg;base64,${b64}`
      return entry.url
    })
    .catch(() => {
      entry.attempts++
      return null
    })
    .finally(() => {
      entry.inflight = undefined
    })
  touch(key, entry)
  return entry.inflight
}

function thumbAttempts(dir: string, file: string): number {
  return thumbCache.get(`${dir}|${file}`)?.attempts ?? 0
}

// Exhausted entries latch so a genuinely broken file isn't re-requested every
// time it scrolls into view. That latch outlives its cause when the failure was
// transient — a slow archive, a burst that timed out — leaving placeholder art
// until the app reloads. retryFailedThumbs drops those entries and wakes every
// mounted card so they try again.
//
// Deliberately wired to the Refresh button alone, not to listing changes: a rev
// bump fires on every part produced and every move landing, so retrying there
// would aim a fresh burst of on-demand ffmpeg at the archive precisely when it
// is already busy — the situation that exhausted the attempts to begin with.
let thumbGeneration = 0
const generationListeners = new Set<() => void>()

export function retryFailedThumbs(): void {
  let cleared = false
  for (const [key, entry] of thumbCache) {
    // Keep loaded thumbnails (re-fetching them is pure waste) and in-flight
    // ones (they haven't failed yet).
    if (entry.url || entry.inflight) continue
    thumbCache.delete(key)
    cleared = true
  }
  if (!cleared) return
  thumbGeneration++
  for (const notify of generationListeners) notify()
}

function subscribeGeneration(onChange: () => void): () => void {
  generationListeners.add(onChange)
  return () => {
    generationListeners.delete(onChange)
  }
}

// LazyThumb fetches the session's screenshot only once the card scrolls into
// view, holding the result in its own state so a thumbnail arriving never
// re-renders anything but this one card. The daemon generates missing
// thumbnails on demand, which can take a moment — hence the
// per-card spinner.
export function LazyThumb({ dir, file }: { dir: string; file: string | null }) {
  const ref = useRef<HTMLDivElement | null>(null)
  const [visible, setVisible] = useState(false)
  const [url, setUrl] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)
  // Bumped to re-run the fetch effect for an automatic retry.
  const [retryTick, setRetryTick] = useState(0)
  // Bumped for every card at once by retryFailedThumbs.
  const generation = useSyncExternalStore(subscribeGeneration, () => thumbGeneration)
  const retryTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  // Track viewport entry/exit. Deliberately not a one-way latch: a card that
  // scrolls away and back re-triggers a fetch if its thumbnail isn't loaded
  // yet (or was evicted from the cache meanwhile).
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const obs = new IntersectionObserver(
      (entries) => setVisible(entries.some((e) => e.isIntersecting)),
      { rootMargin: "200px" },
    )
    obs.observe(el)
    return () => obs.disconnect()
  }, [])

  // A sweep cleared this card's latch: show the spinner again rather than the
  // failed placeholder while the new attempt runs. A no-op on mount.
  useEffect(() => {
    setFailed(false)
  }, [generation])

  useEffect(() => {
    if (!visible || !file || url) return
    let alive = true
    void loadThumb(dir, file).then((loaded) => {
      if (!alive) return
      if (loaded) {
        setUrl(loaded)
        setFailed(false)
        return
      }
      setFailed(true)
      // Retry on-screen cards automatically so every visible thumbnail
      // eventually loads even if an attempt failed (e.g. during a fast-scroll
      // request burst). Off-screen when the timer fires, the effect no-ops
      // until the card is scrolled back into view.
      if (thumbAttempts(dir, file) < MAX_THUMB_ATTEMPTS) {
        clearTimeout(retryTimer.current)
        retryTimer.current = setTimeout(() => setRetryTick((t) => t + 1), RETRY_DELAY_MS)
      }
    })
    return () => {
      alive = false
      clearTimeout(retryTimer.current)
    }
  }, [visible, dir, file, url, retryTick, generation])

  return (
    <div
      ref={ref}
      className="relative flex aspect-video w-full items-center justify-center overflow-hidden rounded-t-xl bg-muted"
    >
      {url ? (
        <img src={url} alt="" className="h-full w-full object-cover" draggable={false} />
      ) : failed || !file ? (
        <Film className="h-8 w-8 text-muted-foreground/50" />
      ) : visible ? (
        <Loader2 className="h-6 w-6 animate-spin text-muted-foreground/50" />
      ) : (
        <Film className="h-8 w-8 text-muted-foreground/30" />
      )}
    </div>
  )
}

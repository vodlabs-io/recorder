import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { useSearchParams } from "react-router-dom"
import { Button } from "@/components/ui/button"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Checkbox } from "@/components/ui/checkbox"
import { Progress } from "@/components/ui/progress"
import { Label } from "@/components/ui/label"
import { ConnectionBanner } from "@/components/connection-banner"
import { LowSpaceBanner } from "@/components/storage-space"
import { UnavailableRootsBanner } from "@/components/unavailable-roots-banner"
import { appStore } from "@/state/app-state"
import { cn } from "@/lib/utils"
import { getDaemonClient, type VOD } from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"
import { Loader2, RefreshCw, Trash2, X } from "lucide-react"
import { DateRangePicker } from "./date-range-picker"
import { FoldersPopover } from "./folders-popover"
import {
  formatBytes,
  formatDate,
  parseLocalDate,
  sessionKey,
  sessionTime,
} from "./format"
import { useRecordings } from "@/state/vods"
import { mergeOutputName, VodCard } from "./vod-card"
import { RecordingInfoSheet } from "./info-sheet"
import { retryFailedThumbs } from "./lazy-thumb"

type SortKey = "newest" | "oldest" | "largest" | "longest"

const SORT_LABELS: Record<SortKey, string> = {
  newest: "Newest",
  oldest: "Oldest",
  largest: "Largest",
  longest: "Longest",
}

// Cards rendered initially and added per growth step. Rendering the whole
// filtered list at once makes a large archive jank (thousands of card
// mounts); a window that grows as the sentinel nears the viewport keeps the
// DOM proportional to what was actually scrolled.
const WINDOW_STEP = 60

// A confirmed bulk delete, snapshotted at the moment the user confirms.
//
// Every figure the progress panel shows is frozen here rather than recomputed
// from the listing, because the listing is exactly what the delete is emptying:
// derived counts would describe a shrinking selection and wind down to "0
// recordings" while the deletion of the original 52 was still running.
type BulkJob = {
  totalCount: number
  // The size of the selection as it was at confirm time; the folders it
  // measures are gone by the time the run finishes adding them up.
  totalBytes: number
  doneCount: number
  doneBytes: number
  failedCount: number
  // Why the first refusal happened. The daemon reports a reason per entry, and
  // "still moving" is only the most common one — a permission or I/O failure
  // reads the same in a bare count and sends the user looking in the wrong
  // place.
  firstError: string | null
  // Succeeded + failed. Drives the bar, so a refused VOD still advances it and
  // it always arrives at 100%, while doneBytes keeps the freed figure honest.
  attemptedBytes: number
  status: "running" | "stopping" | "done"
  // Set when the run ended early: the user's Stop, or a transport failure.
  stoppedReason: string | null
  stoppedByUser: boolean
}

// Keystroke-to-filter delay. Filtering 10k sessions is only a few ms, so this
// mostly avoids re-rendering the grid on every keystroke of a fast typist.
const SEARCH_DEBOUNCE_MS = 150

export default function VODArchive() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"
  const canOpen = getDaemonClient().capabilities.openPath

  const [bulkOpen, setBulkOpen] = useState(false)
  const [bulkJob, setBulkJob] = useState<BulkJob | null>(null)
  // Carries a Stop press into the running loop.
  const stopRef = useRef(false)

  const {
    sessions,
    unavailableRoots,
    daemonScanning,
    loading,
    progress,
    hasLoaded,
    error: loadError,
    refresh,
    removeSessions,
  } = useRecordings(connected, { paused: bulkJob !== null })

  const [search, setSearch] = useState("")
  const [debouncedSearch, setDebouncedSearch] = useState("")
  const [platform, setPlatform] = useState("all")
  // Arriving from the dashboard's VOD count seeds the filter bar and then drops
  // the params: from that point the search box and the platform select own the
  // filter, exactly as if the user had typed and picked them. Seeding the
  // debounced value too, so the grid never flashes the whole archive first.
  const [searchParams, setSearchParams] = useSearchParams()
  useEffect(() => {
    const streamer = searchParams.get("streamer")
    if (!streamer) return
    setSearch(streamer)
    setDebouncedSearch(streamer)
    setPlatform(searchParams.get("platform") || "all")
    setSearchParams({}, { replace: true })
  }, [searchParams, setSearchParams])
  const [fromDate, setFromDate] = useState("")
  const [toDate, setToDate] = useState("")
  const [sort, setSort] = useState<SortKey>("newest")
  // Bumped whenever the Folders popover saves, so the scan-folder set below is
  // re-read rather than describing the configuration as it was on mount.
  const [configRev, setConfigRev] = useState(0)
  // Folders whose VODs are filtered out of the grid, by scan root. Stored as
  // the excluded set rather than the included one so a folder added (or first
  // scanned) while the screen is open shows up instead of being silently
  // filtered away.
  const [hiddenRoots, setHiddenRoots] = useState<Set<string>>(() => new Set())
  // Scan folders, for the read-only VODs whose merge the daemon refuses. Read
  // straight from the config: it is the same list the daemon guards with, and
  // a listed VOD's root is the cleaned path that list holds.
  const [scanRoots, setScanRoots] = useState<Set<string>>(() => new Set())
  useEffect(() => {
    if (!connected) return
    let live = true
    getDaemonClient()
      .getConfig()
      .then((c) => {
        if (live) setScanRoots(new Set(c.scan_dirs ?? []))
      })
      .catch(() => {})
    return () => {
      live = false
    }
  }, [connected, configRev])

  // Only one card's parts list is ever expanded (popped over) at a time.
  const [expandedKey, setExpandedKey] = useState<string | null>(null)
  const onExpandedChange = useCallback((key: string | null) => setExpandedKey(key), [])

  // The info sheet is keyed by dir, not by the VOD object: every refetch
  // replaces those objects, and holding one would freeze the panel on the
  // snapshot it opened with — exactly wrong for a session that is still
  // processing or moving while you watch it. Resolving against the live list
  // also closes the sheet on its own if the session is deleted elsewhere.
  const [infoDir, setInfoDir] = useState<string | null>(null)
  const infoTarget = useMemo(
    () => (infoDir === null ? null : (sessions.find((s) => s.dir === infoDir) ?? null)),
    [sessions, infoDir],
  )
  const onInfo = useCallback((s: VOD) => setInfoDir(s.dir), [])
  const onInfoOpenChange = useCallback((open: boolean) => {
    if (!open) setInfoDir(null)
  }, [])

  // Mirrors the dashboard's VOD-count link: filters the grid down to one
  // streamer's exact username and platform, as if typed and picked by hand.
  // The click can happen anywhere in a long scrolled grid, so also jump back
  // to the top where the filtered results start rendering.
  const onFilterStreamer = useCallback((s: VOD) => {
    if (!s.username) return
    setSearch(s.username)
    setDebouncedSearch(s.username)
    setPlatform(s.platform || "all")
    window.scrollTo({ top: 0 })
  }, [])

  const [deleteTarget, setDeleteTarget] = useState<VOD | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [deleteError, setDeleteError] = useState<string | null>(null)
  const onDelete = useCallback((s: VOD) => setDeleteTarget(s), [])

  const [mergeTarget, setMergeTarget] = useState<VOD | null>(null)
  const [mergeDeleteParts, setMergeDeleteParts] = useState(true)
  const [merging, setMerging] = useState(false)
  const [mergeError, setMergeError] = useState<string | null>(null)
  const onMerge = useCallback((s: VOD) => {
    setMergeDeleteParts(true)
    setMergeTarget(s)
  }, [])

  useEffect(() => {
    const t = setTimeout(() => setDebouncedSearch(search), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(t)
  }, [search])

  const platforms = useMemo(() => {
    const set = new Set<string>()
    for (const s of sessions) {
      if (s.platform) set.add(s.platform)
    }
    return [...set].sort()
  }, [sessions])

  // The VOD Archive's overall date bounds, clamping the range picker's calendar.
  const dateBounds = useMemo(() => {
    let min: number | null = null
    let max: number | null = null
    for (const s of sessions) {
      const iso = sessionTime(s)
      if (!iso) continue
      const t = new Date(iso).getTime()
      if (isNaN(t)) continue
      if (min === null || t < min) min = t
      if (max === null || t > max) max = t
    }
    return {
      min: min !== null ? new Date(min) : undefined,
      max: max !== null ? new Date(max) : undefined,
    }
  }, [sessions])

  const filtered = useMemo(() => {
    const q = debouncedSearch.trim().toLowerCase()
    // Date values are local-midnight boundaries; make "to" inclusive.
    const fromD = fromDate ? parseLocalDate(fromDate) : null
    const toD = toDate ? parseLocalDate(toDate) : null
    const from = fromD ? fromD.getTime() : null
    const to = toD ? toD.getTime() + 24 * 3600 * 1000 : null

    const out = sessions.filter((s) => {
      if (q && !`${s.username} ${s.display_name ?? ""} ${s.platform} ${s.session_id}`.toLowerCase().includes(q)) {
        return false
      }
      if (platform !== "all" && s.platform !== platform) return false
      if (hiddenRoots.size > 0 && hiddenRoots.has(s.root)) return false
      if (from !== null || to !== null) {
        const iso = sessionTime(s)
        const t = iso ? new Date(iso).getTime() : NaN
        if (isNaN(t)) return false
        if (from !== null && t < from) return false
        if (to !== null && t >= to) return false
      }
      return true
    })

    const timeMs = (s: VOD) => {
      const iso = sessionTime(s)
      return iso ? new Date(iso).getTime() : 0
    }
    out.sort((a, b) => {
      switch (sort) {
        case "oldest":
          return timeMs(a) - timeMs(b)
        case "largest":
          return b.total_bytes - a.total_bytes
        case "longest":
          return (b.duration_sec || 0) - (a.duration_sec || 0)
        default:
          return timeMs(b) - timeMs(a)
      }
    })
    return out
  }, [sessions, debouncedSearch, platform, hiddenRoots, fromDate, toDate, sort])

  // Render window: grows as the sentinel scrolls near, snaps back when the
  // result set changes (new filters put the user back at the top anyway).
  const [renderCap, setRenderCap] = useState(WINDOW_STEP)
  useEffect(() => {
    setRenderCap(WINDOW_STEP)
  }, [debouncedSearch, platform, hiddenRoots, fromDate, toDate, sort])
  const visible = filtered.slice(0, renderCap)
  const hasMore = filtered.length > visible.length

  // Re-created whenever the cap grows so the observer re-fires immediately if
  // the sentinel is still within range (tall viewports need several steps to
  // push it out of the margin).
  const [sentinel, setSentinel] = useState<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!sentinel) return
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setRenderCap((c) => c + WINDOW_STEP)
        }
      },
      { rootMargin: "800px" },
    )
    obs.observe(sentinel)
    return () => obs.disconnect()
  }, [sentinel, renderCap])

  // Selection is keyed by dir (a session's identity on disk) rather than by
  // index, so it survives refetches, filter changes and the render window
  // growing underneath it.
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [lastClickedDir, setLastClickedDir] = useState<string | null>(null)

  // Only "ready" VODs can be deleted — the daemon refuses one that is still
  // processing, moving or encoding. Splitting the selection here means the user
  // is told before confirming instead of by a batch of failures afterwards.
  const selectedSessions = useMemo(
    () => filtered.filter((s) => selected.has(s.dir)),
    [filtered, selected],
  )
  const deletable = useMemo(
    () => selectedSessions.filter((s) => s.state === "ready"),
    [selectedSessions],
  )
  const notDeletable = useMemo(
    () => selectedSessions.filter((s) => s.state !== "ready"),
    [selectedSessions],
  )
  const deletableBytes = useMemo(
    () => deletable.reduce((n, s) => n + (s.total_bytes || 0), 0),
    [deletable],
  )

  const toggleSelect = useCallback(
    (s: VOD, shiftKey: boolean) => {
      setSelected((prev) => {
        const next = new Set(prev)
        // Shift extends from the last clicked card across the currently
        // rendered order, which is what the user sees.
        if (shiftKey && lastClickedDir) {
          const order = filtered.slice(0, renderCap)
          const from = order.findIndex((v) => v.dir === lastClickedDir)
          const to = order.findIndex((v) => v.dir === s.dir)
          if (from !== -1 && to !== -1) {
            const [lo, hi] = from < to ? [from, to] : [to, from]
            const select = !prev.has(s.dir)
            for (let i = lo; i <= hi; i++) {
              if (select) next.add(order[i].dir)
              else next.delete(order[i].dir)
            }
            return next
          }
        }
        if (next.has(s.dir)) next.delete(s.dir)
        else next.add(s.dir)
        return next
      })
      setLastClickedDir(s.dir)
    },
    [filtered, renderCap, lastClickedDir],
  )

  // Select-all covers every filtered match, not the rendered window. Selecting
  // 60 of 184 without saying so is the kind of thing noticed only after the
  // confirm, so the count is spelled out on the button too.
  const selectAllFiltered = useCallback(() => {
    setSelected(new Set(filtered.map((s) => s.dir)))
  }, [filtered])

  const clearSelection = useCallback(() => {
    setSelected(new Set())
    setLastClickedDir(null)
  }, [])

  // The span of what is about to go, so a bulk delete is recognisable as the
  // range the user sorted and filtered to.
  const bulkDateSpan = useMemo(() => {
    const times = deletable
      .map((v) => sessionTime(v))
      .filter((t): t is string => Boolean(t))
      .sort()
    if (times.length === 0) return null
    const first = formatDate(times[0])
    const last = formatDate(times[times.length - 1])
    return first === last ? first : `${first} – ${last}`
  }, [deletable])

  const confirmBulkDelete = useCallback(async () => {
    if (deletable.length === 0) return
    const bytesByDir = new Map(deletable.map((v) => [v.dir, v.total_bytes || 0]))
    const dirs = deletable.map((v) => v.dir)
    stopRef.current = false
    setBulkJob({
      totalCount: dirs.length,
      totalBytes: deletableBytes,
      doneCount: 0,
      doneBytes: 0,
      failedCount: 0,
      firstError: null,
      attemptedBytes: 0,
      status: "running",
      stoppedReason: null,
      stoppedByUser: false,
    })

    // One session folder per request. Removing a folder from a network archive
    // can take seconds, so a batch shares a single request ceiling between all
    // of its entries and a slow enough selection times out with nothing
    // reportable; one per request gives each folder the whole budget. It is
    // also what makes the progress bar move per VOD rather than per batch.
    let stopped: string | null = null
    for (const dir of dirs) {
      if (stopRef.current) break
      const bytes = bytesByDir.get(dir) || 0
      try {
        const res = await getDaemonClient().deleteVODs([dir])
        const ok = res.results.some((r) => r.deleted)
        const why = ok ? null : res.results.find((r) => r.error)?.error || null
        // Drop it from the listing here rather than re-pulling: the archive is
        // what is being emptied, so the only correct view of it is the one
        // built from the outcomes as they land.
        if (ok) removeSessions([dir])
        setBulkJob((j) =>
          j && {
            ...j,
            doneCount: j.doneCount + (ok ? 1 : 0),
            doneBytes: j.doneBytes + (ok ? bytes : 0),
            failedCount: j.failedCount + (ok ? 0 : 1),
            firstError: j.firstError ?? why,
            attemptedBytes: j.attemptedBytes + bytes,
          },
        )
      } catch (e) {
        stopped = e instanceof Error ? e.message : String(e)
        break
      }
    }

    const stoppedByUser = stopRef.current
    setBulkJob((j) =>
      j && { ...j, status: "done", stoppedReason: stopped, stoppedByUser },
    )
    // The listing is only re-pulled once the job's own writes are finished, so
    // it reconciles what the daemon actually has instead of racing them.
    await refresh({ force: true })
  }, [deletable, deletableBytes, removeSessions, refresh])

  // Closing the finished dialog is what ends the job: the summary is the only
  // report of a run that can have taken minutes, so it is not dismissed for
  // the user.
  const closeBulkJob = useCallback(() => {
    setBulkJob(null)
    setBulkOpen(false)
    clearSelection()
  }, [clearSelection])

  const requestBulkStop = useCallback(() => {
    stopRef.current = true
    setBulkJob((j) => (j && j.status === "running" ? { ...j, status: "stopping" } : j))
  }, [])

  const confirmDelete = useCallback(async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await getDaemonClient().deleteVOD(deleteTarget.dir)
      setDeleteTarget(null)
      setDeleteError(null)
      await refresh({ force: true })
    } catch (e) {
      setDeleteError(e instanceof Error ? e.message : String(e))
      setDeleteTarget(null)
    } finally {
      setDeleting(false)
    }
  }, [deleteTarget, refresh])

  const confirmMerge = useCallback(async () => {
    if (!mergeTarget) return
    setMerging(true)
    try {
      await getDaemonClient().enqueueMerge(mergeTarget.dir, mergeDeleteParts)
      setMergeTarget(null)
      setMergeError(null)
      // The daemon flips the session to "encoding" and pushes a new snapshot;
      // a refresh isn't required, but it makes the badge appear instantly.
      await refresh({ force: true })
    } catch (e) {
      setMergeError(e instanceof Error ? e.message : String(e))
      setMergeTarget(null)
    } finally {
      setMerging(false)
    }
  }, [mergeTarget, mergeDeleteParts, refresh])

  const error = mergeError ?? deleteError ?? loadError

  // A scan is in flight whenever we're (re)loading, have no listing yet, or
  // the daemon reports a background rescan — regardless of whether stale
  // cached VODs are already showing. Drives the always-visible "Scanning the
  // archive…" indicator.
  const scanning = connected && (loading || !hasLoaded || daemonScanning)

  const deleteFiles = deleteTarget?.files ?? []
  const deleteFileLabel = deleteFiles.length
    ? `${deleteFiles.length} file${deleteFiles.length === 1 ? "" : "s"}`
    : "all files"
  const mergeParts = (mergeTarget?.files ?? []).length

  return (
    <>
      <ConnectionBanner />
      <LowSpaceBanner />
      <UnavailableRootsBanner roots={unavailableRoots} />
      <div className="m-4 flex flex-col gap-4">
        {/* Filter bar */}
        <div className="flex flex-wrap items-center gap-2">
          <InputGroup className="w-48">
            <InputGroupInput
              placeholder="Search streamers…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            {search && (
              <InputGroupAddon align="inline-end">
                {/* Clears the platform too: the pair is usually set together by
                    arriving from a dashboard VOD count, and clearing only half
                    of it leaves the grid narrowed by a filter the user just
                    tried to drop. */}
                <InputGroupButton
                  size="icon-xs"
                  aria-label="Clear filters"
                  onClick={() => {
                    setSearch("")
                    setPlatform("all")
                  }}
                >
                  <X />
                </InputGroupButton>
              </InputGroupAddon>
            )}
          </InputGroup>
          <Select value={platform} onValueChange={(v) => setPlatform(v ?? "all")}>
            <SelectTrigger className="h-8 w-36">
              <SelectValue placeholder="Platform">
                {(value: string | null) =>
                  !value || value === "all" ? "All platforms" : platformDisplayName(value)
                }
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All platforms</SelectItem>
              {platforms.map((p) => (
                <SelectItem key={p} value={p}>
                  {platformDisplayName(p)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <FoldersPopover
            sessions={sessions}
            unavailableRoots={unavailableRoots}
            hidden={hiddenRoots}
            onHiddenChange={setHiddenRoots}
            onFoldersChanged={() => {
              setConfigRev((r) => r + 1)
              // A folder was added or dropped, so the listing is wrong until
              // the daemon walks the new set — not merely stale.
              void refresh({ rescan: true })
            }}
          />
          <DateRangePicker
            from={fromDate}
            to={toDate}
            onChange={(f, t) => {
              setFromDate(f)
              setToDate(t)
            }}
            min={dateBounds.min}
            max={dateBounds.max}
          />
          <Select value={sort} onValueChange={(v) => setSort((v ?? "newest") as SortKey)}>
            <SelectTrigger className="h-8 w-32">
              <SelectValue placeholder="Sort">
                {(value: SortKey | null) => SORT_LABELS[value ?? "newest"]}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="newest">Newest</SelectItem>
              <SelectItem value="oldest">Oldest</SelectItem>
              <SelectItem value="largest">Largest</SelectItem>
              <SelectItem value="longest">Longest</SelectItem>
            </SelectContent>
          </Select>
          {/* Always mounted, only hidden: mounting it would move ml-auto onto
              this span and shove the Refresh button sideways every time a scan
              starts or settles. */}
          <span
            className={cn(
              "ml-auto flex h-8 items-center text-sm text-muted-foreground",
              scanning ? "shimmer" : "invisible",
            )}
            aria-hidden={!scanning}
          >
            {progress
              ? `Loading VODs… ${progress.loaded.toLocaleString()} of ${progress.total.toLocaleString()}`
              : "Scanning for VODs…"}
          </span>
          <Button
            variant="outline"
            size="sm"
            className="h-8"
            onClick={() => {
              // An explicit Refresh is the user saying "try again", so it also
              // clears thumbnails that exhausted their attempts.
              retryFailedThumbs()
              // A walk on a slow archive can run for minutes, and it is exactly
              // then that a user reaches for Refresh. Re-pull the listing so
              // that press does something, but don't ask for a second walk
              // behind the one already running.
              void refresh(scanning ? { force: true } : { rescan: true })
            }}
            disabled={!connected}
          >
            <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} />
            Refresh
          </Button>
        </div>

        {/* While a job runs the bar reports its frozen totals and its buttons
            go inert: the cards it is deleting are leaving the grid underneath,
            so live counts would tick down to "0 selected" and then unmount the
            bar entirely, jumping everything below it. */}
        {(selected.size > 0 || bulkJob) && (
          <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/40 px-3 py-2 text-sm">
            <span className="font-medium tabular-nums">
              {bulkJob ? bulkJob.totalCount : selected.size} selected
              {(bulkJob ? bulkJob.totalBytes : deletableBytes) > 0 && (
                <span className="font-normal text-muted-foreground">
                  {" "}
                  · {formatBytes(bulkJob ? bulkJob.totalBytes : deletableBytes)}
                </span>
              )}
            </span>
            {!bulkJob && selected.size < filtered.length && (
              <Button size="sm" variant="outline" onClick={selectAllFiltered}>
                Select all {filtered.length}
              </Button>
            )}
            <Button size="sm" variant="ghost" onClick={clearSelection} disabled={!!bulkJob}>
              Clear
            </Button>
            <Button
              size="sm"
              variant="destructive"
              className="ml-auto"
              disabled={!!bulkJob || deletable.length === 0}
              onClick={() => setBulkOpen(true)}
            >
              <Trash2 className="h-4 w-4" />
              Delete {bulkJob ? bulkJob.totalCount : deletable.length}
            </Button>
          </div>
        )}

        {error && <p className="text-sm text-destructive">{error}</p>}

        {!connected ? (
          <p className="text-sm text-muted-foreground">Daemon not connected.</p>
        ) : (
          <>
            {bulkJob ? null : !scanning && sessions.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No recorded VODs yet. Finished recordings will appear here.
              </p>
            ) : !scanning && filtered.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No VODs match the current filters.
              </p>
            ) : null}
          </>
        )}

        {/* Session cards. Columns are driven by a card min-width rather than
            viewport breakpoints, so they reflow with the actual grid width
            (which the sidebar eats into) instead of the raw window width.
            The `max(min, container/4)` column basis caps the count at 4 on
            wide screens while still dropping to 3/2/1 as it narrows; raise
            the 17rem min to make cards wider and drop columns sooner. */}
        <div
          className="grid items-start gap-4"
          style={{
            gridTemplateColumns:
              "repeat(auto-fill, minmax(max(18rem, calc((100% - 3 * 1rem) / 4)), 1fr))",
          }}
        >
          {visible.map((s) => {
            const key = sessionKey(s)
            return (
              <VodCard
                key={key}
                session={s}
                canOpen={canOpen}
                readOnly={scanRoots.has(s.root)}
                expanded={expandedKey === key}
                onExpandedChange={onExpandedChange}
                onDelete={onDelete}
                onMerge={onMerge}
                onInfo={onInfo}
                onFilterStreamer={onFilterStreamer}
                selectable
                selected={selected.has(s.dir)}
                onSelectChange={toggleSelect}
              />
            )
          })}
        </div>
        {hasMore && <div ref={setSentinel} className="h-1" aria-hidden />}
      </div>

      <RecordingInfoSheet
        session={infoTarget}
        canOpen={canOpen}
        readOnly={infoTarget !== null && scanRoots.has(infoTarget.root)}
        onOpenChange={onInfoOpenChange}
      />

      {/* Bulk delete. Deletion is permanent by design: trashing on a NAS writes
          into .Trashes on the same volume, so the bytes would not be freed —
          the one thing the operation exists to do. The weight goes in this
          dialog instead of in a recovery path that would not work.

          Three states in one panel: confirm, then progress, then the summary.
          A run over a large selection lasts minutes, so it reports itself here
          rather than closing on the user and leaving a line behind on a page
          they may well have scrolled away from. */}
      <AlertDialog
        open={bulkOpen}
        onOpenChange={(open) => {
          if (open) return
          if (!bulkJob) setBulkOpen(false)
          else if (bulkJob.status === "done") closeBulkJob()
        }}
      >
        <AlertDialogContent>
          {bulkJob ? (
            <BulkDeleteProgress job={bulkJob} />
          ) : (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>
                  Delete {deletable.length} recording
                  {deletable.length === 1 ? "" : "s"}?
                </AlertDialogTitle>
                <AlertDialogDescription>
                  This frees{" "}
                  <span className="font-medium text-foreground">
                    {formatBytes(deletableBytes)}
                  </span>
                  {bulkDateSpan && <> · {bulkDateSpan}</>}
                </AlertDialogDescription>
              </AlertDialogHeader>
              {notDeletable.length > 0 && (
                <p className="rounded-md bg-muted px-2 py-1.5 text-xs text-muted-foreground">
                  {notDeletable.length} of the selected recording
                  {notDeletable.length === 1 ? " is" : "s are"} still being
                  processed, moved or encoded and will be skipped.
                </p>
              )}
              <p className="text-xs text-muted-foreground">
                Whole session folders are deleted. This cannot be undone.
              </p>
            </>
          )}
          <AlertDialogFooter>
            {!bulkJob ? (
              <>
                <AlertDialogCancel>Cancel</AlertDialogCancel>
                <AlertDialogAction
                  onClick={(e) => {
                    e.preventDefault()
                    void confirmBulkDelete()
                  }}
                  className="bg-destructive text-white hover:bg-destructive/90"
                >
                  Delete {deletable.length} permanently
                </AlertDialogAction>
              </>
            ) : bulkJob.status === "done" ? (
              <AlertDialogCancel variant="default" onClick={closeBulkJob}>
                Close
              </AlertDialogCancel>
            ) : (
              <>
                {/* A plain Button, not AlertDialogCancel: that renders a Close
                    and would dismiss the panel out from under a running job. */}
                <Button
                  variant="outline"
                  onClick={requestBulkStop}
                  disabled={bulkJob.status === "stopping"}
                >
                  {bulkJob.status === "stopping" ? "Stopping…" : "Stop"}
                </Button>
                <AlertDialogAction
                  disabled
                  className="bg-destructive text-white hover:bg-destructive/90"
                >
                  <Loader2 className="mr-1 h-4 w-4 animate-spin" />
                  Deleting…
                </AlertDialogAction>
              </>
            )}
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Delete confirmation */}
      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && !deleting && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this recording?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  <span className="font-medium text-foreground">
                    {deleteTarget.username || "Unknown streamer"}
                  </span>{" "}
                  · {formatDate(sessionTime(deleteTarget))} · {deleteFileLabel} ·{" "}
                  {formatBytes(deleteTarget.total_bytes)}
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {deleteTarget && (
            // break-all: a session path is one unbreakable word, so it has to
            // wrap mid-token to stay inside the dialog.
            <p className="rounded-md bg-muted px-2 py-1.5 font-mono text-xs break-all text-muted-foreground">
              {deleteTarget.dir}
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            The whole session folder is deleted. This cannot be undone.
          </p>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault()
                void confirmDelete()
              }}
              disabled={deleting}
              className="bg-destructive text-white hover:bg-destructive/90"
            >
              {deleting && <Loader2 className="mr-1 h-4 w-4 animate-spin" />}
              Delete permanently
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Merge confirmation */}
      <AlertDialog
        open={mergeTarget !== null}
        onOpenChange={(open) => !open && !merging && setMergeTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Merge parts into a single {mergeTarget ? mergeOutputName(mergeTarget) : "file"}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {mergeTarget && (
                <>
                  <span className="font-medium text-foreground">
                    {mergeTarget.username || "Unknown streamer"}
                  </span>{" "}
                  · {formatDate(sessionTime(mergeTarget))} · {mergeParts} parts
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex items-start gap-2">
            <Checkbox
              id="merge-delete-parts"
              checked={mergeDeleteParts}
              onCheckedChange={(v) => setMergeDeleteParts(v === true)}
              disabled={merging}
              className="mt-0.5"
            />
            <div className="grid gap-1">
              <Label htmlFor="merge-delete-parts" className="text-sm font-normal">
                Delete original parts after merge
              </Label>
            </div>
          </div>
          <p className="text-xs text-muted-foreground">
            {mergeTarget && mergeOutputName(mergeTarget) === "vod.webm" &&
              "WebM parts that can't be joined as recorded are re-encoded to vod.mp4. "}
            Monitor progress on the Encoding screen.
          </p>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={merging}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault()
                void confirmMerge()
              }}
              disabled={merging}
            >
              {merging && <Loader2 className="mr-1 h-4 w-4 animate-spin" />}
              Add to queue
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

// BulkDeleteProgress is the running and finished body of the bulk-delete
// dialog. Bytes drive the bar because freeing space is the point of the
// operation, and a selection's VODs differ in size by orders of magnitude —
// counting them would leave the bar sitting still through the large ones and
// leaping through the small.
function BulkDeleteProgress({ job }: { job: BulkJob }) {
  const done = job.status === "done"
  const remaining = job.totalCount - job.doneCount - job.failedCount
  const pct = job.totalBytes > 0
    ? Math.min(100, (job.attemptedBytes / job.totalBytes) * 100)
    : done
      ? 100
      : 0

  let title: string
  if (!done) {
    title = "Deleting recordings…"
  } else if (job.stoppedReason && !job.stoppedByUser) {
    title = `Stopped after ${job.doneCount} of ${job.totalCount}`
  } else if (job.stoppedByUser) {
    title = `Stopped after ${job.doneCount} recording${job.doneCount === 1 ? "" : "s"}`
  } else if (job.failedCount > 0) {
    title = `Deleted ${job.doneCount}, skipped ${job.failedCount}`
  } else {
    title = `Deleted ${job.doneCount} recording${job.doneCount === 1 ? "" : "s"}`
  }

  return (
    <>
      <AlertDialogHeader>
        <AlertDialogTitle>{title}</AlertDialogTitle>
        <AlertDialogDescription className="tabular-nums">
          {done ? (
            <>
              <span className="font-medium text-foreground">
                {formatBytes(job.doneBytes, "0 B")}
              </span>{" "}
              freed
            </>
          ) : (
            <>
              <span className="font-medium text-foreground">
                {formatBytes(job.doneBytes, "0 B")}
              </span>{" "}
              of {formatBytes(job.totalBytes)} freed
            </>
          )}
        </AlertDialogDescription>
      </AlertDialogHeader>
      <div className="flex flex-col gap-1.5">
        <Progress value={pct} />
        {/* tabular-nums: proportional digits change width as they count, which
            makes the line twitch on every update. */}
        <div className="flex justify-between text-xs tabular-nums text-muted-foreground">
          <span>{job.doneCount} deleted</span>
          <span>{done ? `${job.failedCount} skipped` : `${remaining} remaining`}</span>
        </div>
      </div>
      {job.stoppedReason && (
        <p className="text-xs break-words text-destructive">{job.stoppedReason}</p>
      )}
      {done && job.failedCount > 0 && (
        <p className="rounded-md bg-muted px-2 py-1.5 text-xs break-words text-muted-foreground">
          {job.failedCount} recording{job.failedCount === 1 ? "" : "s"} could not be
          deleted{job.firstError ? `: ${job.firstError}` : "."}
        </p>
      )}
    </>
  )
}

import { useRef, useState, type ReactNode } from "react"
import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from "@/components/ui/accordion"
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
import { Card } from "@/components/ui/card"
import RecordingTimer, { formatElapsed } from "@/components/recording-timer"
import { STATUS_BADGE_CLASSES } from "@/components/ui/status-indicator"
import { cn, formatBytes, formatSpeed } from "@/lib/utils"
import { streamerInitials, streamerLabel } from "@shared/streamer-label"
import { platformDisplayName } from "@/data/supported-platforms"
import type { Activity, StreamerView } from "@/lib/daemon"
import { appStore } from "@/state/app-state"
import { activityIndicator } from "./status"
import { ArrowDown, TriangleAlert } from "lucide-react"

// hasCard is true for streamers whose session is actively writing data
// (including the transient "lagging" state), post-processing a finished
// recording (convert/concat), or moving the finished VOD to its destination.
// These are the ones surfaced as cards.
function hasCard(s: StreamerView): boolean {
  return (
    s.activity === "recording" ||
    s.activity === "lagging" ||
    s.activity === "processing" ||
    s.activity === "moving" ||
    s.activity === "moving_retrying"
  )
}

// Problem states sort first so trouble is top-left the moment the grid is
// expanded (the grid counterpart of the strip's anomaly alert); healthy
// recordings next; post-recording busywork trails. Ties keep streamer-list
// order (Array.sort is stable).
const CARD_PRIORITY: Record<string, number> = {
  lagging: 0,
  moving_retrying: 1,
  paused: 2,
  recording: 3,
  processing: 4,
  moving: 5,
}

// isWriting marks the states whose live stats are current; processing/moving
// sessions retain their last flushed speed, which would inflate an aggregate.
function isWriting(s: StreamerView): boolean {
  return s.activity === "recording" || s.activity === "lagging"
}

// TotalSpeed subscribes to the store so only this strip fragment re-renders
// on stats ticks, and sums only actively-writing sessions.
function TotalSpeed({ ids }: { ids: string[] }) {
  const total = appStore((state) =>
    ids.reduce((sum, id) => sum + (state.statsById[id]?.speed ?? 0), 0),
  )
  return (
    <span className="flex items-center gap-0.5 text-xs text-muted-foreground tabular-nums">
      <ArrowDown className="size-3" />
      {formatSpeed(total)}
    </span>
  )
}

// Expanded/collapsed persists per device; first run starts expanded.
const EXPAND_KEY = "vlrec.activeJobsExpanded"

// RecordingTime is the large display: time elapsed since the Session
// started. This can differ from the live duration when recording started after 
// the stream did (e.g. a paused/resumed session, or recovery after lagging).
function RecordingTime({ id, frozen }: { id: string; frozen?: boolean }) {
  const durationMs = appStore((state) => state.statsById[id]?.durationMs)
  // durationMs is the muxer's media duration: it only advances when stats are
  // flushed. Anchor the start once from the first stats and let RecordingTimer
  // tick locally off the wall clock. Re-anchor only when durationMs
  // drops sharply (a new session resetting the duration).
  const startRef = useRef<number | null>(null)
  const prevDurationRef = useRef<number | null>(null)
  if (durationMs != null) {
    const restarted =
      prevDurationRef.current != null &&
      durationMs < prevDurationRef.current - 2000
    if (startRef.current == null || restarted) {
      startRef.current = Date.now() / 1000 - durationMs / 1000
    }
    prevDurationRef.current = durationMs
  }
  // Recording has stopped (post-processing): a locally ticking clock would
  // overcount, so freeze at the final media duration instead.
  const frozenMs = durationMs ?? prevDurationRef.current
  return (
    <div className="font-mono text-xl leading-none tabular-nums">
      {frozen ? (
        frozenMs != null ? formatElapsed(Math.floor(frozenMs / 1000)) : "—"
      ) : startRef.current != null ? (
        <RecordingTimer startUnixSec={startRef.current} />
      ) : (
        "—"
      )}
    </div>
  )
}

// StatsLine subscribes to only its own streamer's live stats, so a stats tick
// re-renders just this line rather than the card.
function StatsLine({
  id,
  lastLiveStart,
}: {
  id: string
  lastLiveStart: number | null
}) {
  const stats = appStore((state) => state.statsById[id])
  return (
    <div className="mt-1 flex items-center justify-between text-xs text-muted-foreground tabular-nums">
      <span>{formatBytes(stats?.totalBytes ?? 0)}</span>
      <span>{formatSpeed(stats?.speed ?? 0)}</span>
      <span className="text-right">
        {lastLiveStart != null ? (
          <RecordingTimer startUnixSec={lastLiveStart} />
        ) : (
          "—"
        )}
      </span>
    </div>
  )
}

// ActivityBadge spells out a card's activity in its tier color, matching the
// table's Status column and the row indicator's dot.
function ActivityBadge({
  activity,
  children,
}: {
  activity: Activity
  children: ReactNode
}) {
  return (
    <Badge
      variant="secondary"
      className={cn(
        "font-normal",
        STATUS_BADGE_CLASSES[activityIndicator(activity).state],
      )}
    >
      {children}
    </Badge>
  )
}

// JobCard surfaces one active job for a streamer: recording, post-processing,
// or moving the finished VOD to its destination. Shows the streamer's avatar
// and identity alongside the real-time stats (elapsed time, bytes written,
// speed). The username acts as a link into the table below, filtering it to
// that streamer; `filtering` means the table is already narrowed to this
// username, so the same click clears it again.
function JobCard({
  s,
  filtering,
  onUsernameClick,
}: {
  s: StreamerView
  filtering: boolean
  onUsernameClick: (username: string, platform: string) => void
}) {
  const recording = s.activity === "recording"
  const lagging = s.activity === "lagging"
  const live_paused = s.activity === "paused"
  const processing = s.activity === "processing"
  const moving = s.activity === "moving"
  const movingRetry = s.activity === "moving_retrying"
  const blockedNoSpace = s.activity === "blocked_no_space"
  const label = streamerLabel(s)
  return (
    <Card className="gap-0 p-3">
      <div className="flex items-center gap-2.5">
        <Avatar className="size-9 shrink-0">
          {s.avatarUrl && <AvatarImage src={s.avatarUrl} alt={label} />}
          <AvatarFallback className="text-xs">
            {streamerInitials(label)}
          </AvatarFallback>
        </Avatar>
        <div className="min-w-0 flex-1">
          {/* The card filters the table by username, which the table's accessor
              matches alongside the label, and by platform so the dropdown
              names what the table is showing. */}
          <button
            type="button"
            title={
              filtering
                ? "Clear the table filter"
                : `Show only ${label} in the table`
            }
            className="block w-full truncate text-left font-medium leading-tight hover:underline"
            onClick={() =>
              onUsernameClick(filtering ? "" : s.username, s.platform)
            }
          >
            <bdi>{label}</bdi>
          </button>
          <p className="truncate text-xs text-muted-foreground">
            {platformDisplayName(s.platform)}
          </p>
        </div>
      </div>

      <div className="mt-2.5 flex items-center justify-between">
        <RecordingTime
          id={`${s.platform}/${s.username}`}
          frozen={processing || moving || movingRetry}
        />
        {recording && (
          // Standard badge; the pulsing red dot is the app's recording motif —
          // the only red on the badge. Soft breathe (opacity fade), calm enough
          // to repeat across a full grid; solid under reduced motion.
          <Badge variant="outline" className="gap-1.5">
            <span className="size-2 rounded-full bg-red-500 motion-safe:animate-rec-breathe" />
            REC
          </Badge>
        )}
        {lagging && <ActivityBadge activity="lagging">Lagging</ActivityBadge>}
        {live_paused && (
          <ActivityBadge activity="paused">Live Paused</ActivityBadge>
        )}
        {processing && (
          <ActivityBadge activity="processing">Processing</ActivityBadge>
        )}
        {moving && <ActivityBadge activity="moving">Moving</ActivityBadge>}
        {movingRetry && (
          <ActivityBadge activity="moving_retrying">
            Moving (retrying)
          </ActivityBadge>
        )}
        {blockedNoSpace && (
          <ActivityBadge activity="blocked_no_space">
            No disk space
          </ActivityBadge>
        )}
      </div>

      <StatsLine
        id={`${s.platform}/${s.username}`}
        lastLiveStart={s.lastLiveStart}
      />
    </Card>
  )
}

// JobCards renders active jobs — in-progress recordings, post-processing,
// and moving finished VODs to their destination — above the table. The
// accordion header doubles as an always-visible summary strip — count, total
// write speed, and an anomaly alert — so the collapsed state stays
// glanceable and tells the user when expanding is worth it. Expansion
// reveals the full grid, problem cards first. It renders nothing when no
// streamer has an active card so the dashboard collapses cleanly to just the
// table.
export function JobCards({
  streamers,
  usernameFilter,
  onUsernameClick,
}: {
  streamers: StreamerView[]
  usernameFilter: string
  onUsernameClick: (username: string, platform: string) => void
}) {
  const [expanded, setExpanded] = useState(
    () => localStorage.getItem(EXPAND_KEY) !== "collapsed",
  )
  const recording = streamers
    .filter(hasCard)
    .sort((a, b) => (CARD_PRIORITY[a.activity] ?? 9) - (CARD_PRIORITY[b.activity] ?? 9))
  if (recording.length === 0) return null

  const writingIds = recording
    .filter(isWriting)
    .map((s) => `${s.platform}/${s.username}`)
  const laggingCount = recording.filter((s) => s.activity === "lagging").length
  const retryingCount = recording.filter(
    (s) => s.activity === "moving_retrying",
  ).length

  return (
    <Accordion
      value={expanded ? ["recordings"] : []}
      onValueChange={(value) => {
        const open = value.length > 0
        setExpanded(open)
        localStorage.setItem(EXPAND_KEY, open ? "expanded" : "collapsed")
      }}
    >
      <AccordionItem value="recordings">
        <AccordionTrigger className="hover:no-underline">
          <span className="flex items-center gap-2.5">
            <span className="group-hover/accordion-trigger:underline">
              Active Jobs
            </span>
            <Badge variant="secondary" className="tabular-nums">
              {recording.length}
            </Badge>
            {writingIds.length > 0 && <TotalSpeed ids={writingIds} />}
            {/* Anomaly alert: only rendered when something needs attention,
                so its mere presence is the signal. */}
            {(laggingCount > 0 || retryingCount > 0) && (
              <span className="flex items-center gap-1 text-xs font-medium text-amber-500">
                <TriangleAlert className="size-3.5" />
                {laggingCount > 0 &&
                  `${laggingCount} lagging`}
                {laggingCount > 0 && retryingCount > 0 && " · "}
                {retryingCount > 0 && `${retryingCount} move retrying`}
              </span>
            )}
          </span>
        </AccordionTrigger>
        <AccordionContent>
          <div className="grid gap-2.5 grid-cols-[repeat(auto-fill,220px)]">
            {recording.map((s) => (
              <JobCard
                key={`${s.platform}/${s.username}`}
                s={s}
                // Matched the way the table matches it — case-insensitively —
                // so a card counts as filtered whenever its own username is
                // what narrowed the table.
                filtering={
                  usernameFilter.toLowerCase() === s.username.toLowerCase()
                }
                onUsernameClick={onUsernameClick}
              />
            ))}
          </div>
        </AccordionContent>
      </AccordionItem>
    </Accordion>
  )
}

export default JobCards

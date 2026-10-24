import * as React from "react"
import { useCallback, useRef } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Popover, PopoverContent } from "@/components/ui/popover"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { getDaemonClient, type VOD } from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"
import { cn } from "@/lib/utils"
import {
  Clapperboard,
  Combine,
  Copy,
  Download,
  EllipsisVertical,
  Film,
  FolderOpen,
  Info,
  Loader2,
  RefreshCw,
  Trash2,
  TriangleAlert,
} from "lucide-react"
import {
  copyText,
  formatBytes,
  formatDurationSec,
  formatRecordedTime,
  moveReasonDetail,
  moveReasonLabel,
  sessionKey,
  sessionTime,
} from "./format"
import { LazyThumb } from "./lazy-thumb"
import { canDownloadVODs, canPlayVODs, downloadVOD, playVOD } from "@/lib/play-vod"

type Props = {
  session: VOD
  canOpen: boolean
  // The VOD sits in a read-only scan folder, so the actions that rewrite its
  // folder are not offered. Delete stays: removing an old recording is the
  // user's call, and "read-only" here means the recorder never writes there on
  // its own, not that the folder is untouchable.
  readOnly: boolean
  // Whether this card's parts popover is open (at most one across the grid).
  expanded: boolean
  // Toggle this card's popover open (key) or closed (null).
  onExpandedChange: (key: string | null) => void
  onDelete: (session: VOD) => void
  onMerge: (session: VOD) => void
  onConvert: (session: VOD) => void
  onInfo: (session: VOD) => void
  // Filters the archive grid down to this session's streamer.
  onFilterStreamer: (session: VOD) => void
  // Selection. selectable turns the checkbox on for the whole grid; selected
  // is this card's state. onSelectChange reports the shift key so the grid can
  // extend a range from the last click.
  selectable: boolean
  selected: boolean
  onSelectChange: (session: VOD, shiftKey: boolean) => void
}

function openFile(s: VOD, file: string): void {
  playVOD(s.dir, file)
}

// A session is mergeable when it has ≥2 numbered parts (vodN.mp4 or
// vodN.webm) — the only shape the daemon's merge job accepts. A session that
// already holds a single merged vod.mp4 or vod.webm has no numbered parts, so
// no Merge item appears.
const partNameRe = /^vod\d+\.(mp4|webm)$/

// mergeOutputName is the file a merge of s starts out writing: vod.webm when
// every part is WebM (joined as recorded), otherwise vod.mp4.
export function mergeOutputName(s: VOD): string {
  const files = s.files ?? []
  return files.length > 0 && files.every((f) => f.name.endsWith(".webm")) ? "vod.webm" : "vod.mp4"
}
function isMergeable(s: VOD): boolean {
  const files = s.files ?? []
  return files.length > 1 && files.every((f) => partNameRe.test(f.name))
}

// isConvertible: the session's only file is vod.webm (VP8, e.g. ManyVids),
// which the daemon's convert job re-encodes to an H.264 vod.mp4.
export function isConvertible(s: VOD): boolean {
  const files = s.files ?? []
  return files.length === 1 && files[0].name === "vod.webm"
}

// VodCard renders one session. Memoized: with per-card thumbnail state inside
// LazyThumb, a grid of thousands of cards re-renders only the card whose
// props actually changed (its popover toggling, or a refetch replacing the
// session object).
export const VodCard = React.memo(function VodCard({
  session: s,
  canOpen,
  readOnly,
  expanded,
  onExpandedChange,
  onDelete,
  onMerge,
  onConvert,
  onInfo,
  onFilterStreamer,
  selectable,
  selected,
  onSelectChange,
}: Props) {
  const key = sessionKey(s)
  // Anchor for the parts-list popover (the whole card).
  const cardRef = useRef<HTMLDivElement | null>(null)
  // Clicking the thumbnail of an open card fires both the popover's
  // outside-press dismissal (on pointerdown) and the button's onClick (on
  // mouseup); without this the click would re-open what the dismissal just
  // closed. Record the dismissal so onCardClick can swallow that click.
  const lastDismissAt = useRef(0)

  const files = s.files ?? []
  const busy = s.state !== "ready"
  // Playing goes to the OS player where there is one, else to the page.
  const canPlay = canPlayVODs()
  const canDownload = canDownloadVODs()
  const clickable = !busy && files.length > 0 && (canPlay || files.length > 1)
  const recordedTime = formatRecordedTime(sessionTime(s))

  const onCardClick = useCallback(() => {
    const files = s.files ?? []
    if (s.state !== "ready" || files.length === 0) return
    if (files.length === 1 && canPlay) {
      openFile(s, files[0].name)
      return
    }
    // Multi-file sessions pop open their part list (and on hosts that can't
    // open files, that's all a click can usefully do). If this card's popover
    // was just dismissed by the pointerdown that began this click, don't
    // re-open it — the click was a toggle-off.
    if (Date.now() - lastDismissAt.current < 350) {
      lastDismissAt.current = 0
      return
    }
    onExpandedChange(expanded ? null : sessionKey(s))
  }, [s, canPlay, expanded, onExpandedChange])

  return (
    <Card ref={cardRef} className="group relative overflow-hidden py-0 gap-0">
      <button
        type="button"
        className={cn(
          "relative block w-full text-left",
          clickable ? "cursor-pointer" : "cursor-default",
        )}
        onClick={onCardClick}
        title={
          !clickable
            ? undefined
            : files.length === 1
              ? canOpen
                ? "Open in video player"
                : "Play"
              : "Show parts"
        }
      >
        {/* A busy session with nothing produced yet asks for any screenshot
            of it (""), which the daemon takes from its raw parts. */}
        <LazyThumb dir={s.dir} file={files[0]?.name ?? (busy ? "" : null)} />
        {files.length > 1 && (
          <Badge
            variant="secondary"
            className="absolute top-1.5 right-1.5 font-normal shadow"
          >
            {files.length} parts
          </Badge>
        )}
      </button>
      {selectable && (
        // Rendered outside the thumbnail button: nesting an interactive control
        // inside a button is invalid, and the click must not open the player.
        <span
          className={cn(
            "absolute top-1.5 left-1.5 rounded-sm bg-background/85 p-1 shadow",
            // Always visible once something is selected, so a scrolled-away
            // selection is never invisible; otherwise reveal on hover/focus.
            selected ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus-within:opacity-100",
          )}
        >
          <Checkbox
            checked={selected}
            aria-label={`Select ${s.display_name || s.username || "recording"}`}
            onClick={(e) => {
              e.stopPropagation()
              onSelectChange(s, (e as React.MouseEvent).shiftKey)
            }}
          />
        </span>
      )}
      <CardContent className="flex flex-col gap-2 p-3">
        <div className="flex items-center justify-between gap-2">
          {s.username ? (
            <button
              type="button"
              className="truncate text-sm font-medium underline decoration-dotted decoration-muted-foreground underline-offset-4 hover:decoration-foreground"
              title={
                s.display_name && s.display_name !== s.username
                  ? `${s.display_name} (${s.username}). Show only this streamer`
                  : `Show only ${s.username}`
              }
              onClick={(e) => {
                e.stopPropagation()
                onFilterStreamer(s)
              }}
            >
              {s.display_name || s.username}
            </button>
          ) : (
            <span className="truncate text-sm font-medium">unknown</span>
          )}
          <span className="flex shrink-0 items-center gap-1">
            {s.platform && (
              <Badge variant="outline" className="font-normal">
                {platformDisplayName(s.platform)}
              </Badge>
            )}
            {s.state === "processing" && (
              <Badge variant="secondary" className="font-normal">
                <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                Processing
              </Badge>
            )}
            {/* The badge stays one short word whatever went wrong: the reason
                is an icon plus a tooltip, since a spelled-out reason would push
                the username out of the card. The banner reports it in full. */}
            {s.state === "moving" && (
              <Badge
                variant="secondary"
                className="font-normal"
                title={moveReasonDetail(s.reason) ?? moveReasonLabel(s.reason)}
              >
                {s.reason ? (
                  <TriangleAlert className="mr-1 h-3 w-3 text-amber-600 dark:text-amber-500" />
                ) : (
                  <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                )}
                Moving
              </Badge>
            )}
            {s.state === "encoding" && (
              <Badge variant="secondary" className="font-normal">
                <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                Encoding
              </Badge>
            )}
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button variant="ghost" size="icon" className="h-6 w-6">
                    <EllipsisVertical className="h-4 w-4" />
                  </Button>
                }
              />
              <DropdownMenuContent align="end" className="min-w-44">
                {/* First because it is the one item that is never gated: every
                    other entry comes and goes with canOpen and the session's
                    state, so anchoring the menu on a stable item keeps it from
                    reshuffling under the pointer between cards. */}
                <DropdownMenuItem onClick={() => onInfo(s)}>
                  <Info className="h-4 w-4" /> Info
                </DropdownMenuItem>
                {canPlay && files.length > 0 && !busy && (
                  <DropdownMenuItem onClick={() => openFile(s, files[0].name)}>
                    <Clapperboard className="h-4 w-4" /> {canOpen ? "Open" : "Play"}
                    {files.length > 1 ? " first part" : ""}
                  </DropdownMenuItem>
                )}
                {canDownload && files.length > 0 && !busy && (
                  <DropdownMenuItem onClick={() => downloadVOD(s.dir, files[0].name)}>
                    <Download className="h-4 w-4" /> Download
                    {files.length > 1 ? " first part" : ""}
                  </DropdownMenuItem>
                )}
                {canOpen && (
                  <DropdownMenuItem
                    onClick={() => void getDaemonClient().openPath(s.dir)}
                  >
                    <FolderOpen className="h-4 w-4" /> Reveal in folder
                  </DropdownMenuItem>
                )}
                <DropdownMenuItem onClick={() => void copyText(s.dir)}>
                  <Copy className="h-4 w-4" /> Copy path
                </DropdownMenuItem>
                {isMergeable(s) && !busy && !readOnly && (
                  <>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem onClick={() => onMerge(s)}>
                      <Combine className="h-4 w-4" /> Merge parts…
                    </DropdownMenuItem>
                  </>
                )}
                {isConvertible(s) && !busy && !readOnly && (
                  <>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem onClick={() => onConvert(s)}>
                      <RefreshCw className="h-4 w-4" /> Convert to MP4…
                    </DropdownMenuItem>
                  </>
                )}
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  variant="destructive"
                  disabled={busy}
                  onClick={() => onDelete(s)}
                >
                  <Trash2 className="h-4 w-4" /> Delete…
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </span>
        </div>
        <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
          <span>
            {recordedTime.date} {recordedTime.time && "• "}
            <span className="whitespace-nowrap">{recordedTime.time}</span>
          </span>
          <span className="shrink-0 whitespace-nowrap">
            {formatDurationSec(s.duration_sec)}
          </span>
          <span className="shrink-0 whitespace-nowrap">{formatBytes(s.total_bytes)}</span>
        </div>

        {files.length > 1 && (
          <Popover
            open={expanded}
            onOpenChange={(open) => {
              if (!open) {
                lastDismissAt.current = Date.now()
                onExpandedChange(null)
              }
            }}
          >
            <PopoverContent
              anchor={cardRef.current}
              side="bottom"
              align="start"
              // Never flip above the card — always drop below it, even when
              // that overflows the viewport (the list scrolls internally,
              // capped at ~6 rows).
              collisionAvoidance={{ side: "none", align: "shift" }}
              // Don't pull focus onto the first part button on open; that
              // paints the global focus ring (see the base-layer outline
              // rule) and flickers as the pointer moves.
              initialFocus={false}
              // Portal-rendered and anchored to the card, so an open popover
              // floats over whatever is below it in the grid instead of
              // pushing it down.
              className="w-(--anchor-width) origin-(--transform-origin) p-1"
            >
              {/* Capped to ~6 rows; longer part lists scroll instead of
                  growing the popover indefinitely. */}
              <div className="flex max-h-44 flex-col divide-y overflow-y-auto">
                {files.map((f, i) => (
                  <button
                    key={f.name}
                    type="button"
                    disabled={!canPlay || busy}
                    onClick={() => openFile(s, f.name)}
                    className={cn(
                      "flex items-center gap-2 whitespace-nowrap px-2 py-1.5 text-left text-xs",
                      canPlay && !busy && "cursor-pointer hover:bg-accent",
                    )}
                    title={
                      canOpen
                        ? `Open in video player (${f.name})`
                        : canPlay
                          ? `Play ${f.name}`
                          : f.name
                    }
                  >
                    <Film className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    <span className="font-mono">Part {i + 1}</span>
                    <span className="ml-auto flex items-center gap-2 text-muted-foreground">
                      <span className="min-w-12 text-right tabular-nums">
                        {formatDurationSec(f.duration_sec)}
                      </span>
                      <span className="min-w-14 text-right tabular-nums">
                        {formatBytes(f.bytes)}
                      </span>
                    </span>
                  </button>
                ))}
              </div>
            </PopoverContent>
          </Popover>
        )}
      </CardContent>
    </Card>
  )
})

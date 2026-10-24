import * as React from "react"
import { useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet"
import { getDaemonClient, type VOD } from "@/lib/daemon"
import {
  platformDisplayName,
  streamerChannelURL,
} from "@/data/supported-platforms"
import { cn } from "@/lib/utils"
import {
  Check,
  Clapperboard,
  Copy,
  Download,
  ExternalLink,
  Film,
  FolderOpen,
  Loader2,
  TriangleAlert,
} from "lucide-react"
import {
  copyText,
  formatBitrate,
  formatBytes,
  formatDate,
  formatDurationSec,
  formatTimeOfDay,
  gapSec,
  moveReasonDetail,
  moveReasonLabel,
  sessionTime,
  spanSec,
} from "./format"
import { LazyThumb } from "./lazy-thumb"
import { canDownloadVODs, canPlayVODs, downloadVOD, playVOD } from "@/lib/play-vod"

type Props = {
  // The session to describe, or null when the sheet is closed. Held by the
  // grid rather than by each card: one sheet serves the whole archive, so a
  // listing of thousands of cards doesn't mount thousands of dialogs.
  session: VOD | null
  canOpen: boolean
  // The VOD sits in a read-only scan folder. A scan folder is not a Storage
  // root — nothing is written to it and its free space is never measured — so
  // the Location section names it for what it is.
  readOnly: boolean
  onOpenChange: (open: boolean) => void
}

// Row is one label/value line. Values are right-aligned and tabular so the
// numbers down the panel line up on their digits.
function Row({
  label,
  value,
  title,
  muted,
  warn,
}: {
  label: string
  value: React.ReactNode
  title?: string
  muted?: boolean
  warn?: boolean
}) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span
        className={cn(
          "min-w-0 truncate text-right tabular-nums",
          muted && "text-muted-foreground",
          warn && "text-amber-600 dark:text-amber-500",
        )}
        title={title}
      >
        {value}
      </span>
    </div>
  )
}

function Section({
  title,
  children,
}: {
  title: string
  children: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-2">
      <h3 className="text-xs font-medium tracking-wide text-muted-foreground/80 uppercase">
        {title}
      </h3>
      <div className="flex flex-col gap-1.5 text-sm">{children}</div>
    </section>
  )
}

// CopyButton reports back after a copy: the clipboard gives no feedback of its
// own, and a button that looks identical before and after leaves you unsure
// whether the click registered.
function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)
  // Reset when the path changes under a sheet that stayed open (reopened onto
  // a different session), so a stale tick isn't left showing.
  useEffect(() => setCopied(false), [text])
  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(t)
  }, [copied])

  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label={label}
      title={label}
      onClick={() => {
        void copyText(text)
        setCopied(true)
      }}
    >
      {copied ? (
        <Check className="h-3.5 w-3.5 text-emerald-600 dark:text-emerald-500" />
      ) : (
        <Copy className="h-3.5 w-3.5" />
      )}
    </Button>
  )
}

function StateBadge({ s }: { s: VOD }) {
  if (s.state === "ready") return null
  if (s.state === "moving") {
    return (
      <Badge variant="secondary" className="font-normal">
        {moveReasonLabel(s.reason)}
      </Badge>
    )
  }
  return (
    <Badge variant="secondary" className="font-normal">
      <Loader2 className="mr-1 h-3 w-3 animate-spin" />
      {s.state === "processing" ? "Processing" : "Encoding"}
    </Badge>
  )
}

// RecordingInfoSheet is the full read-out for one session: what the card shows
// at a glance, plus the things it has no room for — the exact timestamps, the
// per-part breakdown, where the bytes actually live, and how much of the
// stream's wall-clock span the recording is missing.
export function RecordingInfoSheet({
  session: s,
  canOpen,
  readOnly,
  onOpenChange,
}: Props) {
  return (
    <Sheet open={s !== null} onOpenChange={onOpenChange}>
      {/* Wider than the sheet's default sm: the part table needs four columns.
          Same variant prefix as the base class so tailwind-merge replaces it
          rather than leaving both max-widths to fight in the cascade. */}
      <SheetContent
        side="right"
        className="data-[side=right]:sm:max-w-md"
        aria-describedby={undefined}
      >
        {s && <InfoBody s={s} canOpen={canOpen} readOnly={readOnly} />}
      </SheetContent>
    </Sheet>
  )
}

function InfoBody({
  s,
  canOpen,
  readOnly,
}: {
  s: VOD
  canOpen: boolean
  readOnly: boolean
}) {
  const files = s.files ?? []
  const busy = s.state !== "ready"
  const canPlay = canPlayVODs()
  const canDownload = canDownloadVODs()
  const playable = canPlay && !busy && files.length > 0
  const channelURL = s.username ? streamerChannelURL(s.platform, s.username) : null

  const span = spanSec(s)
  const gap = gapSec(s)
  const detail = s.state === "moving" ? moveReasonDetail(s.reason) : undefined
  const titles = s.titles ?? []

  return (
    <>
      {/* pr-10 keeps the title clear of the sheet's own close button. */}
      <SheetHeader className="gap-1 pr-10 pb-0">
        <SheetTitle className="truncate">
          {s.display_name || s.username || "Unknown streamer"}
        </SheetTitle>
        <SheetDescription className="flex flex-wrap items-center gap-x-2 gap-y-1">
          {s.platform && <span>{platformDisplayName(s.platform)}</span>}
          {s.platform && <span aria-hidden>·</span>}
          <span>{formatDate(sessionTime(s))}</span>
        </SheetDescription>
      </SheetHeader>

      <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-4 pb-4">
        {/* shrink-0: an aspect-ratio box is a flex item with a min-content
            height of zero, so without it a panel whose content overflows
            squashes the thumbnail to a strip instead of scrolling. */}
        <div className="shrink-0 overflow-hidden rounded-lg border">
          <LazyThumb dir={s.dir} file={files.length > 0 ? files[0].name : null} />
        </div>

        {busy && (
          <div className="flex flex-col gap-1.5">
            <StateBadge s={s} />
            {detail && <p className="text-xs text-muted-foreground">{detail}</p>}
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          {playable && (
            <Button onClick={() => playVOD(s.dir, files[0].name)}>
              <Clapperboard className="h-4 w-4" />
              Play{files.length > 1 ? " part 1" : ""}
            </Button>
          )}
          {canDownload && !busy && files.length > 0 && (
            <Button
              variant="outline"
              onClick={() => downloadVOD(s.dir, files[0].name)}
            >
              <Download className="h-4 w-4" />
              Download{files.length > 1 ? " part 1" : ""}
            </Button>
          )}
          {canOpen && (
            <Button
              variant="outline"
              onClick={() => void getDaemonClient().openPath(s.dir)}
            >
              <FolderOpen className="h-4 w-4" />
              Reveal
            </Button>
          )}
          {channelURL && (
            <Button
              variant="outline"
              onClick={() => void getDaemonClient().openExternal(channelURL)}
            >
              <ExternalLink className="h-4 w-4" />
              Channel
            </Button>
          )}
        </div>

        {titles.length > 0 && (
          // The one part of the panel written by a person rather than measured,
          // so it wraps rather than truncates. A streamer retitles mid-broadcast
          // — often several times over a long one — and each title is stamped
          // with when the recorder first saw it, which is also roughly where in
          // the recording it starts.
          <Section title={titles.length === 1 ? "Stream title" : "Stream titles"}>
            {titles.length === 1 ? (
              <p className="text-sm break-words">{titles[0].text}</p>
            ) : (
              <div className="flex flex-col divide-y rounded-md border">
                {titles.map((t, i) => (
                  <div
                    key={`${t.at ?? i}-${t.text}`}
                    className="flex items-baseline gap-2.5 px-2.5 py-2 text-xs"
                  >
                    <span className="min-w-14 shrink-0 text-right tabular-nums text-muted-foreground">
                      {formatTimeOfDay(t.at)}
                    </span>
                    <span className="min-w-0 flex-1 break-words">{t.text}</span>
                  </div>
                ))}
              </div>
            )}
          </Section>
        )}

        <Section title="Recording">
          <Row label="Duration" value={formatDurationSec(s.duration_sec)} />
          <Row label="Size" value={formatBytes(s.total_bytes)} />
          <Row
            label="Average bitrate"
            value={formatBitrate(s.total_bytes, s.duration_sec)}
          />
          <Row
            label="Files"
            value={
              files.length === 0
                ? "none yet"
                : files.length === 1
                  ? "1 file"
                  : `${files.length} parts`
            }
          />
        </Section>

        <Section title="Timeline">
          <Row label="Started" value={s.start ? formatDate(s.start) : "—"} />
          <Row label="Ended" value={s.end ? formatDate(s.end) : "—"} />
          <Row
            label="Wall clock"
            value={span === undefined ? "—" : formatDurationSec(span)}
            title="Time from the first frame to the last, including any stretch spent reconnecting."
          />
          {gap !== undefined && (
            <>
              <Row label="Not recorded" value={formatDurationSec(gap)} warn />
              {/* The one number here a user cannot get anywhere else, so it
                  gets a sentence rather than being left to be puzzled over. */}
              <p className="flex gap-1.5 text-xs text-muted-foreground">
                <TriangleAlert className="mt-px h-3.5 w-3.5 shrink-0 text-amber-600 dark:text-amber-500" />
                <span>
                  Some footage could not be recorded due to an upstream pause or network lag.
                </span>
              </p>
            </>
          )}
        </Section>

        {files.length > 0 && (
          <Section title={files.length === 1 ? "File" : "Parts"}>
            <div className="flex flex-col divide-y rounded-md border">
              {files.map((f, i) => {
                const openable = canPlay && !busy
                return (
                  <div key={f.name} className="flex items-center">
                  <button
                    type="button"
                    disabled={!openable}
                    onClick={() => playVOD(s.dir, f.name)}
                    className={cn(
                      "flex min-w-0 flex-1 items-center gap-2 px-2.5 py-2 text-left text-xs",
                      openable && "cursor-pointer hover:bg-accent",
                    )}
                    title={
                      !openable
                        ? f.name
                        : canOpen
                          ? `Open in video player (${f.name})`
                          : `Play ${f.name}`
                    }
                  >
                    <Film className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 flex-1 truncate font-mono">{f.name}</span>
                    <span className="flex shrink-0 items-center gap-2.5 tabular-nums text-muted-foreground">
                      <span className="min-w-14 text-right">
                        {formatTimeOfDay(f.start)}
                      </span>
                      <span className="min-w-12 text-right">
                        {formatDurationSec(f.duration_sec)}
                      </span>
                      <span className="min-w-14 text-right">{formatBytes(f.bytes)}</span>
                    </span>
                  </button>
                  {canDownload && !busy && (
                    <Button
                      variant="ghost"
                      size="icon"
                      className="mr-1 h-7 w-7 shrink-0"
                      title={`Download ${f.name}`}
                      onClick={() => downloadVOD(s.dir, f.name)}
                    >
                      <Download className="h-3.5 w-3.5" />
                    </Button>
                  )}
                  </div>
                )
              })}
            </div>
          </Section>
        )}

        <Section title="Location">
          {/* The path wraps rather than truncates: it is the thing people come
              to this panel to read off, and a nested archive path is long. */}
          <div className="flex items-start gap-1 rounded-md bg-muted px-2.5 py-2">
            <span className="min-w-0 flex-1 font-mono text-xs break-all">{s.dir}</span>
            <CopyButton text={s.dir} label="Copy folder path" />
          </div>
          <Row
            label={readOnly ? "Scan folder" : "Storage root"}
            value={s.root || "—"}
            title={s.root}
            muted
          />
          {readOnly && (
            <p className="text-xs text-muted-foreground">
              Read-only: nothing is written here, and this VOD's parts cannot be
              merged in place.
            </p>
          )}
        </Section>

        <Section title="Details">
          <Row
            label="Session ID"
            value={s.session_id || "—"}
            title={s.session_id}
            muted
          />
          <Row
            label="Source"
            value={s.from_manifest ? "Session manifest" : "Scanned from disk"}
            muted
          />
          {!s.from_manifest && (
            // Without a manifest the scanner infers what it can from the folder
            // layout and file mtimes, so blanks and rough times here are the
            // expected result rather than a fault worth chasing.
            <p className="text-xs text-muted-foreground">
              This folder has no session manifest. The streamer, platform and times
              above were recovered from the folder layout and file timestamps, so some
              may be missing or approximate.
            </p>
          )}
        </Section>
      </div>
    </>
  )
}

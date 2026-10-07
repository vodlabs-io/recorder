import * as React from "react"
import {
  columnFilteringFeature,
  columnVisibilityFeature,
  createFilteredRowModel,
  createPaginatedRowModel,
  createSortedRowModel,
  filterFn_equalsString,
  filterFn_includesString,
  metaHelper,
  rowPaginationFeature,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_text,
  tableFeatures,
  type ColumnDef,
  type SortDirection,
} from "@tanstack/react-table"
import {
  ChevronDown,
  ChevronsUpDown,
  ChevronUp,
  MoreHorizontal,
  Eye,
  Trash2,
  RefreshCw,
  SlidersHorizontal,
  CircleSlash,
} from "lucide-react"
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import {
  AlertDialog,
  AlertDialogTrigger,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogAction,
  AlertDialogCancel,
} from "@/components/ui/alert-dialog"
import StatusIndicator, {
  STATUS_BADGE_CLASSES,
} from "@/components/ui/status-indicator"
import { cn, relativeTimeFromUnix } from "@/lib/utils"
import { streamerInitials, streamerLabel } from "@shared/streamer-label"
import { refPayload } from "@shared/daemon/subjects"
import {
  NO_MONITOR_REASON,
  platformDisplayName,
  platformNoMonitor,
  streamerChannelURL,
} from "@/data/supported-platforms"
import {
  getDaemonClient,
  type SetStreamerOptionsParams,
  type StreamerView,
} from "@/lib/daemon"
import {
  STATUS_LABELS,
  STATUS_SORT_ORDER,
  isMoving,
  oneShotBlockedReason,
  oneShotEligible,
  statusKey,
  streamerIndicator,
} from "./status"

// dashboardFeatures is the dashboard table's feature set. Filter and sort
// functions resolve only if registered here, including the ones "auto" picks:
// includesString for the search box, alphanumeric/text for string columns.
export const dashboardFeatures = tableFeatures({
  columnFilteringFeature,
  columnVisibilityFeature,
  rowPaginationFeature,
  rowSortingFeature,
  filteredRowModel: createFilteredRowModel(),
  paginatedRowModel: createPaginatedRowModel(),
  sortedRowModel: createSortedRowModel(),
  filterFns: {
    equalsString: filterFn_equalsString,
    includesString: filterFn_includesString,
  },
  sortFns: { alphanumeric: sortFn_alphanumeric, text: sortFn_text },
  // Per-column cell styling.
  columnMeta: metaHelper<{ cellClassName?: string }>(),
})

const client = getDaemonClient()

// streamerRef narrows a StreamerView to the single identity form the daemon
// accepts — a request carrying both platform/username and url is refused
// outright, and a resolved streamer's doc can hold each of them.
const streamerRef = refPayload

// optionsParams builds the complete option set for set_options, which replaces
// every managed option at once: a field left out is read as "inherit", so an
// inline change to one option must restate all the others to preserve them.
function optionsParams(
  s: StreamerView,
  override: Partial<SetStreamerOptionsParams>,
): SetStreamerOptionsParams {
  return {
    ...streamerRef(s),
    monitor: s.monitor,
    variant: s.variant,
    output_template: s.outputTemplate,
    delete_part_files: s.deletePartFiles,
    split_size: s.splitSize,
    split_duration: s.splitDuration,
    archive_dir: s.archiveDir,
    ...override,
  }
}

// columnSortChevron matches the Accounts table: a dimmed up/down chevron marks
// a sortable-but-unsorted column, replaced by the active direction once sorted.
function columnSortChevron(sorted: false | SortDirection): React.ReactNode {
  if (!sorted) return <ChevronsUpDown className="h-3.5 w-3.5 opacity-40" />
  return sorted === "asc" ? (
    <ChevronUp className="h-3.5 w-3.5" />
  ) : (
    <ChevronDown className="h-3.5 w-3.5" />
  )
}

function sortableHeader(label: string) {
  return ({ column }: any) => {
    const sorted = column.getIsSorted()
    return (
      <button
        type="button"
        className={cn(
          "inline-flex items-center gap-1 hover:text-foreground",
          sorted ? "text-foreground" : "text-muted-foreground",
        )}
        onClick={() => column.toggleSorting(sorted === "asc")}
      >
        {label}
        {columnSortChevron(sorted)}
      </button>
    )
  }
}

function StatusCell({ s }: { s: StreamerView }) {
  const { state } = streamerIndicator(s)
  return (
    <Badge
      variant="secondary"
      className={cn("font-normal", STATUS_BADGE_CLASSES[state])}
    >
      {STATUS_LABELS[statusKey(s)]}
    </Badge>
  )
}

// RemoveStreamerButton is the row's trash action plus its confirmation.
function RemoveStreamerButton({
  s,
  disabled,
}: {
  s: StreamerView
  disabled: boolean
}) {
  const [open, setOpen] = React.useState(false)
  return (
    <AlertDialog open={open} onOpenChange={setOpen}>
      <Tooltip>
        <TooltipTrigger
          render={
            <AlertDialogTrigger
              render={
                <Button
                  variant="ghost"
                  className="h-8 w-8 p-0 text-destructive hover:text-destructive"
                  disabled={disabled}
                >
                  <span className="sr-only">Remove</span>
                  <Trash2 />
                </Button>
              }
            />
          }
        />
        <TooltipContent>Remove</TooltipContent>
      </Tooltip>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove streamer?</AlertDialogTitle>
          <AlertDialogDescription>
            This will stop recording <bdi>{streamerLabel(s)}</bdi>. Are you
            sure?
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            className="bg-destructive"
            onClick={() => {
              client.removeStreamer(streamerRef(s))
              setOpen(false)
            }}
          >
            Yes, remove
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// DashboardRow is a streamer plus the figures the table derives rather than
// reads from the daemon. vodCount rides the row data instead of being looked up
// in the column's accessor because TanStack caches a row's accessor values on
// first read and rebuilds rows only when `data` changes: a count read from a
// closure would freeze at whatever the archive listing held the first time the
// column was sorted, and no later count would dislodge it.
export type DashboardRow = StreamerView & { vodCount: number }

// vodArchiveHref links to the VOD Archive seeded with one streamer's filter.
// The archive filters on identity, not on the label: a display name is mutable
// and not unique.
function vodArchiveHref(s: StreamerView): string {
  const q = new URLSearchParams({ platform: s.platform, streamer: s.username })
  return `#/vods?${q.toString()}`
}

// buildColumns assembles the dashboard table's column definitions. `disabled`
// reflects daemon connectivity (gating the row actions); `onEditOptions` opens
// the per-streamer options dialog owned by the Dashboard screen. `vodsLoaded`
// says whether the rows' vodCount can be trusted yet — an archive still
// downloading would otherwise show every streamer as 0.
export function buildColumns({
  disabled,
  onEditOptions,
  onCancelMove,
  vodsLoaded,
}: {
  disabled: boolean
  onEditOptions: (s: StreamerView) => void
  onCancelMove: (s: StreamerView) => void
  vodsLoaded: boolean
}): ColumnDef<typeof dashboardFeatures, DashboardRow>[] {
  return [
    {
      id: "indicator",
      enableHiding: false,
      enableSorting: false,
      meta: { cellClassName: "w-4 pr-0" },
      header: () => null,
      cell: ({ row }) => {
        const { state, tooltip } = streamerIndicator(row.original)
        return (
          <div className="flex justify-center pl-1">
            <Tooltip>
              <TooltipTrigger
                render={
                  <span>
                    <StatusIndicator size="sm" state={state} />
                  </span>
                }
              />
              {tooltip && <TooltipContent>{tooltip}</TooltipContent>}
            </Tooltip>
          </div>
        )
      },
    },
    {
      id: "username",
      // Matches the label AND the username, so a display name typed from the
      // screen and an id pasted from a URL both find the row — and the
      // click-to-filter on the recording cards, which filters by the exact
      // username, keeps working on platforms that have a display name.
      accessorFn: (row) => [row.displayName, row.username].filter(Boolean).join(" "),
      enableHiding: false,
      meta: { cellClassName: "w-full max-w-0" },
      header: sortableHeader("Streamer"),
      cell: ({ row }) => {
        const s = row.original
        const label = streamerLabel(s)
        return (
          <div className="flex items-center gap-2">
            <Avatar className={!s.record ? "opacity-40" : ""}>
              {s.avatarUrl && <AvatarImage src={s.avatarUrl} alt={label} />}
              <AvatarFallback>{streamerInitials(label)}</AvatarFallback>
            </Avatar>
            {/* bdi keeps an RTL name from reordering the columns drawn after
                it; title exposes the stable id the label replaced. */}
            <bdi
              className={`min-w-0 overflow-hidden text-ellipsis whitespace-nowrap ${
                !s.record ? "text-muted-foreground" : ""
              }`}
              title={label === s.username ? undefined : s.username}
            >
              {label}
            </bdi>
          </div>
        )
      },
    },
    {
      id: "platform",
      accessorFn: (row) => row.platform,
      // The platform dropdown picks one exact platform; the default substring
      // match would let "tiktok" also select a future "tiktok-live".
      filterFn: "equalsString",
      header: sortableHeader("Platform"),
      cell: ({ row }) => <div>{platformDisplayName(row.original.platform)}</div>,
    },
    {
      id: "status",
      accessorFn: (row) => statusKey(row),
      enableHiding: false,
      header: sortableHeader("Status"),
      sortFn: (a, b) =>
        STATUS_SORT_ORDER.indexOf(statusKey(a.original)) -
        STATUS_SORT_ORDER.indexOf(statusKey(b.original)),
      cell: ({ row }) => <StatusCell s={row.original} />,
    },
    {
      id: "last_live",
      accessorFn: (row) => row.lastLiveStart ?? 0,
      header: sortableHeader("Last Live"),
      // Blank only when the time is genuinely unknown — a live streamer's
      // current run is the last time it went live, recording or not.
      cell: ({ row }) => relativeTimeFromUnix(row.original.lastLiveStart),
    },
    {
      id: "vods",
      accessorFn: (row) => row.vodCount,
      header: sortableHeader("VODs"),
      cell: ({ row }) => {
        const s = row.original
        const count = s.vodCount
        // An em dash until the archive listing has arrived, so "not counted
        // yet" doesn't read as "nothing recorded".
        if (!vodsLoaded) return <span className="text-muted-foreground">—</span>
        if (count === 0) return <span className="text-muted-foreground">0</span>
        // An anchor, like the sidebar's nav items: this leaves the screen, so
        // it should carry a link's cursor, its middle-click and its "link" role
        // rather than a button's. A dotted underline does the marking — the
        // palette is neutral, so a link colour would be indistinguishable from
        // the static "0" in the rows next to it.
        return (
          <a
            href={vodArchiveHref(s)}
            // Padded out to a 24px-square target ("1" is only ~8px of glyph),
            // with the matching negative margin so the digits stay exactly
            // where the column aligns them. The padding is deliberately
            // unpainted: the pointer cursor already appears across it.
            className="-mx-2 inline-flex min-w-6 items-center justify-center px-2 py-1 underline decoration-dotted decoration-muted-foreground underline-offset-4 hover:decoration-foreground"
          >
            {count}
          </a>
        )
      },
    },
    {
      id: "added",
      accessorFn: (row) => row.addedAt ?? 0,
      header: sortableHeader("Added"),
      cell: ({ row }) => relativeTimeFromUnix(row.original.addedAt),
    },
    {
      id: "monitor",
      accessorFn: (row) => row.monitor,
      header: sortableHeader("Monitor"),
      cell: ({ row }) => {
        const s = row.original
        // Switching monitoring off turns the streamer into a one-shot, which
        // captures the stream running right now and is then done with it. The
        // daemon refuses that for anyone not live and recording, so the switch
        // is locked rather than left to fail silently. Switching it back ON is
        // always allowed — that is how a one-shot is kept.
        const blocked =
          (s.monitor && !oneShotEligible(s)) ||
          (!s.monitor && platformNoMonitor(s.platform))
        const toggle = (
          <Switch
            checked={s.monitor}
            disabled={disabled || blocked}
            onCheckedChange={(monitor) =>
              client.setStreamerOptions(optionsParams(s, { monitor }))
            }
          />
        )
        return (
          <div className="flex w-15 justify-center">
            {blocked ? (
              // A disabled switch swallows its own hover events, so the
              // tooltip hangs off a wrapper.
              <Tooltip>
                <TooltipTrigger render={<span>{toggle}</span>} />
                <TooltipContent>
                  {s.monitor ? oneShotBlockedReason(s) : NO_MONITOR_REASON}
                </TooltipContent>
              </Tooltip>
            ) : (
              toggle
            )}
          </div>
        )
      },
    },
    {
      id: "record",
      accessorFn: (row) => row.record,
      header: sortableHeader("Record"),
      cell: ({ row }) => {
        const s = row.original
        // Recording only switches for a monitored streamer: a one-shot
        // (monitor=false) exists to capture the run it was added for.
        const locked = disabled || !s.monitor
        const toggle = (
          <Switch
            checked={s.record}
            disabled={locked}
            onCheckedChange={(record) =>
              record
                ? client.enableStreamer(streamerRef(s))
                : client.disableStreamer(streamerRef(s))
            }
          />
        )
        return (
          <div className="flex w-15 justify-center">
            {s.monitor ? (
              toggle
            ) : (
              // A disabled switch swallows its own hover events, so the
              // tooltip hangs off a wrapper.
              <Tooltip>
                <TooltipTrigger render={<span>{toggle}</span>} />
                <TooltipContent>
                  {platformNoMonitor(s.platform)
                    ? "Recorded once. Remove it to stop recording."
                    : "Recorded once. Remove it to stop recording, or enable monitoring first."}
                </TooltipContent>
              </Tooltip>
            )}
          </div>
        )
      },
    },
    {
      id: "actions",
      enableHiding: false,
      header: "Actions",
      cell: ({ row }) => {
        const s = row.original
        const channelURL = streamerChannelURL(s.platform, s.username)
        return (
          <div className="flex items-center justify-end gap-1">
            <RemoveStreamerButton s={s} disabled={disabled} />

            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    variant="ghost"
                    className="h-8 w-10 p-0"
                    disabled={disabled}
                  >
                    <span className="sr-only">Open menu</span>
                    <MoreHorizontal />
                  </Button>
                }
              />
              <DropdownMenuContent align="end" className="min-w-44">
                <DropdownMenuItem onClick={() => onEditOptions(s)}>
                  <SlidersHorizontal />
                  <span>Edit options</span>
                </DropdownMenuItem>
                {/* On a retired streamer the daemon turns this into a re-check
                    that also clears the retirement — the only way back from
                    Defunct. It decides, not us: it owns the documents this
                    table renders, so it can never be reading a staler status
                    than the row the user just clicked. */}
                <DropdownMenuItem
                  onClick={() => client.checkStreamer(streamerRef(s))}
                >
                  <RefreshCw />
                  <span>Check now</span>
                </DropdownMenuItem>
                {/* The streamer's own page URL is not stored anywhere: an add
                    resolves a pasted URL to platform/username and discards it.
                    The link is built from the platform's live-page template
                    instead, which the daemon supplies for every platform. */}
                {channelURL && (
                  <DropdownMenuItem
                    onClick={() => client.openExternal(channelURL)}
                  >
                    <Eye />
                    <span>Open in browser</span>
                  </DropdownMenuItem>
                )}
                {/* Only while something is actually on its way to the archive
                    folder: cancelling is one-way, so it is not offered as a
                    standing option. The dialog the screen owns explains what
                    stays where. */}
                {isMoving(s) && (
                  <DropdownMenuItem
                    variant="destructive"
                    onClick={() => onCancelMove(s)}
                  >
                    <CircleSlash />
                    <span>Cancel move</span>
                  </DropdownMenuItem>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        )
      },
    },
  ]
}

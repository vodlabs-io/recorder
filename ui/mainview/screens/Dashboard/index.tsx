import * as React from "react"
import {
  ColumnFiltersState,
  flexRender,
  getCoreRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  SortingState,
  useReactTable,
  VisibilityState,
} from "@tanstack/react-table"
import { ChevronDown, CirclePlay, CircleSlash, Trash2, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogAction,
  AlertDialogCancel,
} from "@/components/ui/alert-dialog"
import { ConnectionBanner } from "@/components/connection-banner"
import { RegistrationBanner } from "@/components/registration-banner"
import { ServiceBanner } from "@/components/service-banner"
import { UpdateRequiredBanner } from "@/components/update-required-banner"
import { StreamerOptionsDialog } from "@/components/streamer-options-dialog"
import { appStore } from "@/state/app-state"
import { DEFAULT_COLUMN_VIS } from "@/state/prefs"
import { useVodCounts, vodCountKey } from "@/state/vods"
import { getDaemonClient, type StreamerView } from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"
import AddStreamer from "../AddStreamer"
import { buildColumns, type DashboardRow } from "./columns"
import { DashboardEmpty } from "./empty-state"
import { JobCards } from "./job-cards"
import { isMoving } from "./status"

const client = getDaemonClient()

// Survives navigation within the session but not app restarts.
let sessionSorting: SortingState | null = null
const DEFAULT_SORTING: SortingState = [{ id: "added", desc: true }]

function columnTitleForId(columnId: string): string {
  switch (columnId) {
    case "last_live":
      return "Last Live"
    case "vods":
      return "VODs"
    default:
      return columnId
  }
}

function Dashboard() {
  const streamerList = appStore((state) => state.streamers)
  const connection = appStore((state) => state.connection)
  const disabled = connection.state !== "connected"
  const { counts: vodCounts, hasLoaded: vodsLoaded } = useVodCounts(!disabled)

  const [confirmRemoveAllOpen, setConfirmRemoveAllOpen] = React.useState(false)
  const [actionsMenuOpen, setActionsMenuOpen] = React.useState(false)
  // Which move cancellation is awaiting confirmation: one streamer, "all", or
  // nothing pending. One piece of state drives both entry points, since the
  // warning they need is identical.
  const [cancelMoveTarget, setCancelMoveTarget] = React.useState<
    StreamerView | "all" | null
  >(null)
  const [optionsStreamer, setOptionsStreamer] =
    React.useState<StreamerView | null>(null)
  const [optionsOpen, setOptionsOpen] = React.useState(false)
  const [sorting, setSorting] = React.useState<SortingState>(
    sessionSorting ?? DEFAULT_SORTING,
  )
  const [columnFilters, setColumnFilters] = React.useState<ColumnFiltersState>(
    [],
  )
  const [columnVisibility, setColumnVisibility] =
    React.useState<VisibilityState>(DEFAULT_COLUMN_VIS)
  const [appliedColumnVisPrefs, setAppliedColumnVisPrefs] =
    React.useState(false)
  const prefsHydrated = appStore((state) => state.prefsHydrated)
  const persistPrefs = appStore((state) => state.persistPrefs)

  React.useEffect(() => {
    const state = appStore.getState()
    if (state.prefsHydrated && !appliedColumnVisPrefs) {
      const vis = state.prefs.dashboard_column_visibility
      if (vis) setColumnVisibility(vis as VisibilityState)
      setAppliedColumnVisPrefs(true)
    }
  }, [prefsHydrated, appliedColumnVisPrefs])

  function openOptions(s: StreamerView) {
    setOptionsStreamer(s)
    setOptionsOpen(true)
  }

  // The count is folded into the row rather than looked up in the column's
  // accessor: see DashboardRow. A new count map is therefore a new `data`
  // array, which is what makes the table re-read the column.
  const rows = React.useMemo<DashboardRow[]>(
    () =>
      streamerList.map((s) => ({
        ...s,
        vodCount: vodCounts.get(vodCountKey(s.platform, s.username)) ?? 0,
      })),
    [streamerList, vodCounts],
  )

  const columns = React.useMemo(
    () =>
      buildColumns({
        disabled,
        onEditOptions: openOptions,
        onCancelMove: setCancelMoveTarget,
        vodsLoaded,
      }),
    [disabled, vodsLoaded],
  )

  // Whether anything is on its way to an archive folder, which is the only time
  // "Cancel all moves" has something to act on.
  const anyMoving = React.useMemo(
    () => streamerList.some(isMoving),
    [streamerList],
  )

  async function confirmCancelMoves() {
    const target = cancelMoveTarget
    setCancelMoveTarget(null)
    if (!target) return
    await client.cancelMoves(
      target === "all"
        ? undefined
        : { platform: target.platform, username: target.username },
    )
  }

  // Only the platforms actually present in the list are offered as filters.
  const platforms = React.useMemo(() => {
    const set = new Set<string>()
    for (const s of streamerList) {
      if (s.platform) set.add(s.platform)
    }
    return [...set].sort()
  }, [streamerList])

  function handleRemoveAllClicked() {
    setActionsMenuOpen(false)
    if (streamerList.length > 0) setConfirmRemoveAllOpen(true)
  }

  function handleDisableAllClicked() {
    setActionsMenuOpen(false)
    client.disableAllStreamers()
  }

  function handleEnableAllClicked() {
    setActionsMenuOpen(false)
    client.enableAllStreamers()
  }

  const table = useReactTable<DashboardRow>({
    data: rows,
    columns,
    onSortingChange: (updater) => {
      const next =
        typeof updater === "function" ? updater(sorting) : updater
      sessionSorting = next
      setSorting(next)
    },
    onColumnFiltersChange: setColumnFilters,
    getCoreRowModel: getCoreRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    onColumnVisibilityChange: (updater) => {
      const nextState =
        typeof updater === "function" ? updater(columnVisibility) : updater
      setColumnVisibility(nextState)
      persistPrefs({ dashboard_column_visibility: nextState })
    },
    state: { sorting, columnFilters, columnVisibility },
    initialState: { pagination: { pageIndex: 0, pageSize: 8 } },
    // A streamer going live/offline or having an option toggled replaces the
    // `data` array; without this the page index would snap back to 0, bouncing
    // the user off their current page whenever a row's state changes.
    autoResetPageIndex: false,
  })

  const currentPage = table.getState().pagination.pageIndex + 1
  const totalPages = Math.max(1, table.getPageCount())

  const usernameFilter =
    (table.getColumn("username")?.getFilterValue() as string) ?? ""
  const platformFilter =
    (table.getColumn("platform")?.getFilterValue() as string) ?? "all"

  // Narrowing the rows can leave the current page past the end of the result
  // set (autoResetPageIndex is off so row-state churn doesn't bounce the user),
  // so every filter change goes back to page one.
  function applyFilter(columnId: string, value: string | undefined) {
    table.getColumn(columnId)?.setFilterValue(value)
    table.setPageIndex(0)
  }

  // The search box's clear button drops the platform too, matching the VOD
  // Archive: the two are usually set together, and clearing only half of it
  // leaves the table narrowed by a filter the user just tried to drop.
  function clearFilters() {
    table.getColumn("username")?.setFilterValue("")
    table.getColumn("platform")?.setFilterValue(undefined)
    table.setPageIndex(0)
  }

  // A card's username jumps the table to that streamer, selecting its platform
  // too so the dropdown names what the table is showing — and so a platform
  // left on some other value can't leave the jump showing an empty table. The
  // same click again (empty username) drops both, as the clear button does.
  function jumpToUsername(username: string, platform: string) {
    table.getColumn("platform")?.setFilterValue(username ? platform : undefined)
    applyFilter("username", username)
  }

  return (
    <>
      <ConnectionBanner />
      <UpdateRequiredBanner />
      <ServiceBanner />
      <RegistrationBanner />
      {!disabled && streamerList.length === 0 && <DashboardEmpty />}
      {!disabled && streamerList.length > 0 && (
        <>
          {/* max-w-table is the app-wide table ceiling (see index.css); the
              cards, toolbar and pagination share it so they stay flush with
              the table's edges. */}
          <div className="mx-auto flex w-full max-w-table flex-col overflow-y-auto px-4 text-sm">
            <JobCards
              streamers={streamerList}
              usernameFilter={usernameFilter}
              onUsernameClick={jumpToUsername}
            />
            <div className="flex items-center gap-2 py-4">
              <InputGroup className="max-w-50">
                <InputGroupInput
                  placeholder="Search streamers..."
                  value={usernameFilter}
                  onChange={(event) =>
                    applyFilter("username", event.target.value)
                  }
                />
                {usernameFilter && (
                  <InputGroupAddon align="inline-end">
                    <InputGroupButton
                      size="icon-xs"
                      aria-label="Clear filters"
                      onClick={clearFilters}
                    >
                      <X />
                    </InputGroupButton>
                  </InputGroupAddon>
                )}
              </InputGroup>
              <Select
                value={platformFilter}
                onValueChange={(value) =>
                  applyFilter(
                    "platform",
                    !value || value === "all" ? undefined : value,
                  )
                }
              >
                <SelectTrigger className="w-36">
                  <SelectValue placeholder="Platform">
                    {(value: string | null) =>
                      !value || value === "all"
                        ? "All platforms"
                        : platformDisplayName(value)
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
              <div className="ml-auto flex items-center space-x-2">
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <Button variant="outline" className="ml-auto">
                        Columns <ChevronDown />
                      </Button>
                    }
                  />
                  <DropdownMenuContent align="end">
                    {table
                      .getAllColumns()
                      .filter((column) => column.getCanHide())
                      .map((column) => (
                        <DropdownMenuCheckboxItem
                          key={column.id}
                          className="capitalize"
                          checked={column.getIsVisible()}
                          onCheckedChange={(value) =>
                            column.toggleVisibility(!!value)
                          }
                        >
                          {columnTitleForId(column.id)}
                        </DropdownMenuCheckboxItem>
                      ))}
                  </DropdownMenuContent>
                </DropdownMenu>

                <DropdownMenu open={actionsMenuOpen} onOpenChange={setActionsMenuOpen}>
                  <DropdownMenuTrigger
                    render={
                      <Button variant="outline" className="ml-auto" disabled={disabled}>
                        Actions <ChevronDown />
                      </Button>
                    }
                  />
                  <DropdownMenuContent>
                    <DropdownMenuItem onClick={handleDisableAllClicked}>
                      <CircleSlash />
                      <span>Disable All</span>
                    </DropdownMenuItem>
                    <DropdownMenuItem onClick={handleEnableAllClicked}>
                      <CirclePlay />
                      <span>Enable All</span>
                    </DropdownMenuItem>
                    {anyMoving && (
                      <DropdownMenuItem
                        variant="destructive"
                        onClick={() => {
                          setActionsMenuOpen(false)
                          setCancelMoveTarget("all")
                        }}
                      >
                        <CircleSlash />
                        <span>Cancel All Moves</span>
                      </DropdownMenuItem>
                    )}
                    <DropdownMenuItem
                      variant="destructive"
                      onClick={handleRemoveAllClicked}
                    >
                      <Trash2 />
                      <span>Remove All</span>
                    </DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>

                <AddStreamer disabled={disabled} />

                <AlertDialog
                  open={confirmRemoveAllOpen}
                  onOpenChange={setConfirmRemoveAllOpen}
                >
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>Remove all streamers?</AlertDialogTitle>
                      <AlertDialogDescription>
                        This will remove {streamerList.length} streamer
                        {streamerList.length === 1 ? "" : "s"}. Are you sure?
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>Cancel</AlertDialogCancel>
                      <AlertDialogAction
                        onClick={() => {
                          client.removeAllStreamers()
                          setConfirmRemoveAllOpen(false)
                        }}
                        className="bg-destructive"
                      >
                        Yes, remove all
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>

                <AlertDialog
                  open={cancelMoveTarget !== null}
                  onOpenChange={(open) => {
                    if (!open) setCancelMoveTarget(null)
                  }}
                >
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>
                        {cancelMoveTarget === "all"
                          ? "Cancel all moves?"
                          : `Cancel move for ${cancelMoveTarget?.username ?? ""}?`}
                      </AlertDialogTitle>
                      <AlertDialogDescription>
                        Stop automatically moving their recordings to the archive 
                        folder. You'll need to manually transfer any partially-moved 
                        files yourself.
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>Keep moving</AlertDialogCancel>
                      <AlertDialogAction
                        onClick={confirmCancelMoves}
                        className="bg-destructive"
                      >
                        {cancelMoveTarget === "all"
                          ? "Yes, cancel all"
                          : "Yes, cancel"}
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              </div>
            </div>
            <div className="rounded-md border">
              <Table>
                <TableHeader>
                  {table.getHeaderGroups().map((headerGroup) => (
                    <TableRow key={headerGroup.id}>
                      {headerGroup.headers.map((header) => (
                        <TableHead key={header.id}>
                          {header.isPlaceholder
                            ? null
                            : flexRender(
                                header.column.columnDef.header,
                                header.getContext(),
                              )}
                        </TableHead>
                      ))}
                    </TableRow>
                  ))}
                </TableHeader>
                <TableBody>
                  {table.getRowModel().rows?.length ? (
                    table.getRowModel().rows.map((row) => (
                      <TableRow key={`${row.original.platform}/${row.original.username}`}>
                        {row.getVisibleCells().map((cell) => (
                          <TableCell
                            key={cell.id}
                            className={cell.column.columnDef.meta?.cellClassName}
                          >
                            {flexRender(
                              cell.column.columnDef.cell,
                              cell.getContext(),
                            )}
                          </TableCell>
                        ))}
                      </TableRow>
                    ))
                  ) : (
                    <TableRow>
                      <TableCell
                        colSpan={columns.length}
                        className="h-24 text-center text-muted-foreground"
                      >
                        {disabled ? "Not connected to daemon" : "No streamers"}
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </div>

            <div className="flex items-center justify-end space-x-2 py-4">
              <div className="text-muted-foreground flex-1 text-sm">
                {currentPage} / {totalPages}
              </div>
              <div className="space-x-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => table.previousPage()}
                  disabled={!table.getCanPreviousPage()}
                >
                  Previous
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => table.nextPage()}
                  disabled={!table.getCanNextPage()}
                >
                  Next
                </Button>
              </div>
            </div>
          </div>

          <StreamerOptionsDialog
            streamer={optionsStreamer}
            open={optionsOpen}
            onOpenChange={setOptionsOpen}
          />
        </>
      )}
    </>
  )
}

export default Dashboard

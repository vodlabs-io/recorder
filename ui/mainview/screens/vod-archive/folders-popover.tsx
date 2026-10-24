import { useCallback, useEffect, useMemo, useState } from "react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"
import { FolderDatalist, useFolderSuggestions } from "@/lib/folder-suggestions"
import { getDaemonClient, type VOD, type UserConfig } from "@/lib/daemon"
import {
  AlertTriangle,
  Archive,
  Download,
  Folders,
  Loader2,
  Lock,
  Plus,
  X,
} from "lucide-react"

// FolderRole says why a folder is in the listing, which is also what decides
// whether it can be removed here: only a scan folder is ours to unconfigure.
type FolderRole = "download" | "archive" | "scan"

type FolderRow = {
  path: string
  role: FolderRole
  count: number
  unavailable: boolean
  // A scan folder the config actually lists, as opposed to one only inferred
  // from the VODs on screen. Removing is offered for the former only.
  removable: boolean
}

const ROLE_ICON = {
  download: Download,
  archive: Archive,
  scan: Lock,
} as const

const ROLE_LABEL: Record<FolderRole, string> = {
  download: "Download folder",
  archive: "Archive folder",
  scan: "Scan folder · read-only",
}

// Order folders by what they are, not by name: the two folders the daemon
// writes to are the ones a user is looking for, and burying them among a dozen
// old disks would make the common case the hard one.
const ROLE_ORDER: Record<FolderRole, number> = {
  download: 0,
  archive: 1,
  scan: 2,
}

type Props = {
  // The listing, for per-folder VOD counts.
  sessions: VOD[]
  // Roots the last scan could not walk (an unplugged drive, an offline NAS).
  unavailableRoots: string[]
  // Folders whose VODs are filtered out of the grid. Held by the parent so the
  // filter survives the popover closing.
  hidden: Set<string>
  onHiddenChange: (hidden: Set<string>) => void
  // Called after the scan folder list is saved, so the archive re-walks.
  onFoldersChanged: () => void
}

// FoldersPopover lists every folder the VOD Archive was built from — the
// download folder, the archive folders, and the read-only scan folders — with
// what each contributed and whether it could be read at all. It doubles as the
// place scan folders are added and removed: the setting exists only to change
// this screen, and Settings → Files is about where recordings are written.
export function FoldersPopover({
  sessions,
  unavailableRoots,
  hidden,
  onHiddenChange,
  onFoldersChanged,
}: Props) {
  const canChoose = getDaemonClient().capabilities.chooseDirectory
  const [open, setOpen] = useState(false)
  const [cfg, setCfg] = useState<UserConfig | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Free-text path entry, for hosts without a native picker (the Web UI, whose
  // daemon may not even be on this machine).
  const [typedPath, setTypedPath] = useState("")
  const folders = useFolderSuggestions()

  const loadConfig = useCallback(async () => {
    try {
      setCfg(await getDaemonClient().getConfig())
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    if (!open) return
    setError(null)
    void loadConfig()
  }, [open, loadConfig])

  const counts = useMemo(() => {
    const m = new Map<string, number>()
    for (const s of sessions) {
      if (!s.root) continue
      m.set(s.root, (m.get(s.root) ?? 0) + 1)
    }
    return m
  }, [sessions])

  const rows = useMemo(
    () => buildRows(cfg, counts, unavailableRoots),
    [cfg, counts, unavailableRoots],
  )

  // Every mutation re-reads the config first: this popover owns one field of a
  // whole-object save, and writing a copy loaded when it opened would revert
  // anything Settings changed in between.
  const saveScanDirs = useCallback(
    async (next: string[]) => {
      setSaving(true)
      setError(null)
      try {
        const fresh = await getDaemonClient().getConfig()
        setCfg(await getDaemonClient().setConfig({ ...fresh, scan_dirs: next }))
        setTypedPath("")
        onFoldersChanged()
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
        // The daemon rejected or never saw the change; show what it actually holds.
        void loadConfig()
      } finally {
        setSaving(false)
      }
    },
    [loadConfig, onFoldersChanged],
  )

  async function addPicked() {
    const dir = await getDaemonClient().chooseDirectory()
    if (dir) void saveScanDirs([...(cfg?.scan_dirs ?? []), dir])
  }

  function addTyped() {
    const dir = typedPath.trim()
    if (dir) void saveScanDirs([...(cfg?.scan_dirs ?? []), dir])
  }

  function remove(path: string) {
    void saveScanDirs((cfg?.scan_dirs ?? []).filter((d) => d !== path))
  }

  function toggle(path: string, show: boolean) {
    const next = new Set(hidden)
    if (show) next.delete(path)
    else next.add(path)
    onHiddenChange(next)
  }

  const shownCount = rows.filter((r) => !hidden.has(r.path)).length
  const filtering = shownCount < rows.length

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button variant="outline" size="sm" className="h-8">
            <Folders className="h-4 w-4" />
            {filtering ? `Folders ${shownCount}/${rows.length}` : "Folders"}
            {unavailableRoots.length > 0 && (
              <AlertTriangle className="h-3.5 w-3.5 text-amber-500" />
            )}
          </Button>
        }
      />
      <PopoverContent align="start" className="w-96 gap-2">
        {rows.length === 0 ? (
          <p className="px-1 py-2 text-sm text-muted-foreground">
            {cfg ? "No folders configured." : "Loading folders…"}
          </p>
        ) : (
          <ul className="flex flex-col">
            {rows.map((r) => {
              const Icon = ROLE_ICON[r.role]
              return (
                <li
                  key={r.path}
                  className="flex items-start gap-2 rounded-md px-1 py-1.5 hover:bg-accent/50"
                >
                  <Checkbox
                    className="mt-0.5"
                    checked={!hidden.has(r.path)}
                    onCheckedChange={(v) => toggle(r.path, v === true)}
                    aria-label={`Show VODs in ${r.path}`}
                  />
                  <div className="min-w-0 flex-1">
                    <PathLabel path={r.path} />
                    <p className="flex items-center gap-1 text-xs text-muted-foreground">
                      <Icon className="h-3 w-3 shrink-0" />
                      <span>{ROLE_LABEL[r.role]}</span>
                      <span aria-hidden>·</span>
                      {r.unavailable ? (
                        <span className="text-amber-600 dark:text-amber-500">
                          unavailable
                        </span>
                      ) : (
                        <span className="tabular-nums">
                          {r.count.toLocaleString()} VOD{r.count === 1 ? "" : "s"}
                        </span>
                      )}
                    </p>
                  </div>
                  {r.removable && (
                    <Tooltip>
                      <TooltipTrigger
                        render={
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6 shrink-0"
                            disabled={saving}
                            aria-label={`Stop scanning ${r.path}`}
                            onClick={() => remove(r.path)}
                          >
                            <X className="h-3.5 w-3.5" />
                          </Button>
                        }
                      />
                      <TooltipContent>
                        Stop scanning. Nothing on disk is deleted.
                      </TooltipContent>
                    </Tooltip>
                  )}
                </li>
              )
            })}
          </ul>
        )}

        <div className="border-t pt-2">
          {canChoose ? (
            <Button
              variant="outline"
              size="sm"
              className="w-full"
              disabled={saving || !cfg}
              onClick={() => void addPicked()}
            >
              {saving ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Plus className="h-4 w-4" />
              )}
              Add scan folder…
            </Button>
          ) : (
            <div className="flex items-center gap-2">
              <Input
                className="h-8 flex-1 font-mono text-xs"
                placeholder="/mnt/olddisk/vods"
                value={typedPath}
                disabled={saving || !cfg}
                onChange={(e) => setTypedPath(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") addTyped()
                }}
                list="scan-dir-folders"
              />
              <FolderDatalist id="scan-dir-folders" folders={folders} />
              <Button
                variant="outline"
                size="sm"
                className="h-8"
                disabled={saving || !cfg || typedPath.trim() === ""}
                onClick={addTyped}
              >
                {saving ? <Loader2 className="h-4 w-4 animate-spin" /> : "Add"}
              </Button>
            </div>
          )}
          <p className={cn("mt-2 px-1 text-xs text-muted-foreground")}>
            A scan folder is only read: recordings are never moved into one, and
            its VODs cannot be merged.
          </p>
          {error && <p className="mt-2 px-1 text-xs text-destructive">{error}</p>}
        </div>
      </PopoverContent>
    </Popover>
  )
}

// PathLabel keeps the END of a path on screen. Ordinary truncation cuts the
// only part that tells two folders apart — sibling folders under one mount
// share every leading character, so "/var/folders/nx/plb1tznd…" renders
// identically for all of them.
function PathLabel({ path }: { path: string }) {
  const cut = Math.max(path.lastIndexOf("/"), path.lastIndexOf("\\"))
  const head = cut > 0 ? path.slice(0, cut) : ""
  const tail = cut > 0 ? path.slice(cut) : path
  return (
    <p className="flex font-mono text-xs" title={path}>
      <span className="truncate">{head}</span>
      <span className="shrink-0">{tail}</span>
    </p>
  )
}

// buildRows merges what the config declares with what the listing actually came
// from. The union matters in both directions: a per-streamer archive folder is
// a real root that the global config never mentions, and a folder just added
// has no VODs on screen until the next scan finishes.
function buildRows(
  cfg: UserConfig | null,
  counts: Map<string, number>,
  unavailableRoots: string[],
): FolderRow[] {
  const unavailable = new Set(unavailableRoots)
  const rows = new Map<string, FolderRow>()

  const put = (path: string, role: FolderRole, removable: boolean) => {
    if (!path || rows.has(path)) return
    rows.set(path, {
      path,
      role,
      count: counts.get(path) ?? 0,
      unavailable: unavailable.has(path),
      removable,
    })
  }

  if (cfg) {
    put(cfg.download_dir, "download", false)
    put(cfg.archive_dir, "archive", false)
    for (const d of cfg.scan_dirs ?? []) put(d, "scan", true)
  }
  // Anything else the scan reported. A root with VODs but no config entry is a
  // per-streamer or one-shot archive folder, which is a write destination.
  for (const path of counts.keys()) put(path, "archive", false)
  for (const path of unavailable) put(path, "archive", false)

  return [...rows.values()].sort(
    (a, b) => ROLE_ORDER[a.role] - ROLE_ORDER[b.role] || a.path.localeCompare(b.path),
  )
}

export default FoldersPopover

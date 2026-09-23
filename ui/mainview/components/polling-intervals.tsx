import { useCallback, useEffect, useState } from "react"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { cn } from "@/lib/utils"
import { appStore } from "@/state/app-state"
import { getDaemonClient, type CloudIntervals } from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"

// formatDuration renders seconds as a compact "Xm YYs" / "Xh Ym" / "Xs" string.
function formatDuration(totalSec: number): string {
  if (totalSec < 60) return `${totalSec}s`
  const h = Math.floor(totalSec / 3600)
  const m = Math.floor((totalSec % 3600) / 60)
  const s = totalSec % 60
  if (h > 0) return `${h}h ${m}m`
  return `${m}m ${s.toString().padStart(2, "0")}s`
}

// healthColor maps an estimate against the target to a dot color: green when at
// or under target, amber up to 2× target, red beyond, slate when unknown.
function healthColor(estSec: number | null, targetSec: number): string {
  if (estSec === null || targetSec <= 0) return "bg-slate-500"
  if (estSec <= targetSec) return "bg-green-500"
  if (estSec <= 2 * targetSec) return "bg-amber-500"
  return "bg-red-500"
}

export function PollingIntervals() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"
  const daemon = appStore((s) => s.daemon)

  const [data, setData] = useState<CloudIntervals | null>(null)
  const [error, setError] = useState<string | null>(null)

  const fetchIntervals = useCallback(async () => {
    try {
      const res = await getDaemonClient().getCloudIntervals()
      setData(res)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  // Fetch on mount (once connected) and whenever the window regains focus, so the
  // numbers refresh both when switching to this tab and when returning to the app.
  useEffect(() => {
    if (!connected) return
    fetchIntervals()
    const onFocus = () => fetchIntervals()
    window.addEventListener("focus", onFocus)
    return () => window.removeEventListener("focus", onFocus)
  }, [connected, fetchIntervals])

  const targetSec = data?.target_seconds ?? 0

  return (
    <div className="flex flex-col gap-4">
        <div className="flex flex-col gap-2 text-sm">
          <span className="font-medium">How often streamers are checked</span>

          {/* Two short columns, so this one stays well inside the app-wide
              max-w-table ceiling (see index.css). */}
          <div className="mx-auto w-full max-w-sm rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Platform</TableHead>
                  <TableHead className="text-right">Interval</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {!data || data.intervals.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={2}
                      className="h-24 text-center text-muted-foreground"
                    >
                      {!data
                        ? `${connected ? "Loading polling intervals…" : "Daemon not connected."}${error ? ` ${error}` : ""}`
                        : "No platforms reported."}
                    </TableCell>
                  </TableRow>
                ) : (
                  data.intervals.map((pi) => (
                    <TableRow key={pi.platform}>
                      <TableCell>
                        <span className="flex items-center gap-2">
                          <span
                            className={cn(
                              "inline-flex h-2.5 w-2.5 rounded-full",
                              healthColor(pi.estimated_seconds, targetSec),
                            )}
                          />
                          <span className="font-medium">
                            {platformDisplayName(pi.platform)}
                          </span>
                        </span>
                      </TableCell>
                      <TableCell className="text-right font-mono tabular-nums">
                        {pi.estimated_seconds === null
                          ? "unknown"
                          : formatDuration(pi.estimated_seconds)}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>

          {data && error && (
            <p className="text-xs text-destructive">Failed to refresh: {error}</p>
          )}
        </div>

        {connected &&
          (daemon.serverVersion || daemon.serverStartedAt !== null) && (
            <p className="mt-auto self-end text-xs text-muted-foreground">
              {daemon.serverVersion && <>backend {daemon.serverVersion}</>}
              {daemon.serverStartedAt !== null && (
                <>
                  {daemon.serverVersion && " · "}
                  up{" "}
                  {formatDuration(
                    Math.max(
                      0,
                      Math.floor(Date.now() / 1000) - daemon.serverStartedAt,
                    ),
                  )}
                </>
              )}
            </p>
          )}
    </div>
  )
}


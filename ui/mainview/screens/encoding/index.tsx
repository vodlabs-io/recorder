import { useCallback, useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { Banner } from "@/components/banner"
import { ConnectionBanner } from "@/components/connection-banner"
import { appStore } from "@/state/app-state"
import {
  getDaemonClient,
  type EncodeJob,
  type EncodeProgress,
} from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"
import { joinPath } from "@/lib/utils"
import {
  AlertTriangle,
  ChevronDown,
  ChevronUp,
  ChevronsUp,
  Loader2,
  Pause,
  Play,
  RotateCcw,
  Trash2,
  X,
} from "lucide-react"

// The Encoding screen lists the daemon's merge/re-encode job queue: the
// running job with live progress, queued jobs (reorderable), and recent
// history. Queue contents re-pull whenever snapshot.encode.queueRev moves;
// the running job's progress streams via snapshot.encode.progress.

function basename(dir: string): string {
  const parts = dir.split(/[\\/]/).filter(Boolean)
  return parts[parts.length - 1] ?? dir
}

function jobLabel(j: EncodeJob): string {
  if (j.platform && j.username) {
    return `${platformDisplayName(j.platform)} / ${j.username}`
  }
  return basename(j.dir)
}

function formatEta(sec: number): string {
  if (sec <= 0) return ""
  if (sec < 60) return `${sec}s`
  const m = Math.floor(sec / 60)
  if (m < 60) return `${m}m ${sec % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

function statusBadge(j: EncodeJob) {
  switch (j.status) {
    case "running":
      return (
        <Badge variant="secondary" className="font-normal">
          <Loader2 className="mr-1 h-3 w-3 animate-spin" />
          {j.stage === "copy"
            ? "Copying"
            : j.stage === "finalize"
              ? "Finalizing"
              : "Encoding"}
        </Badge>
      )
    case "queued":
      return (
        <Badge variant="outline" className="font-normal">
          Queued
        </Badge>
      )
    case "done":
      return (
        <Badge variant="secondary" className="font-normal">
          Done
        </Badge>
      )
    case "failed":
      return (
        <Badge variant="destructive" className="font-normal">
          Failed
        </Badge>
      )
    default:
      return (
        <Badge variant="outline" className="font-normal">
          Cancelled
        </Badge>
      )
  }
}

// RunningProgress renders the live bar for the running job: encode-stage
// percent/ETA/speed, or copy-stage byte percent ("Copying to storage").
function RunningProgress({ progress }: { progress: EncodeProgress | null }) {
  const indeterminate = !progress || progress.percent < 0
  const value = indeterminate ? null : Math.min(progress.percent, 100)
  const detail = (() => {
    if (!progress) return "Starting…"
    if (progress.stage === "copy") {
      return indeterminate
        ? "Copying to storage…"
        : `Copying to storage… ${Math.floor(progress.percent)}%`
    }
    const bits: string[] = []
    if (!indeterminate) bits.push(`${Math.floor(progress.percent)}%`)
    if (progress.eta_sec) bits.push(`${formatEta(progress.eta_sec)} left`)
    if (progress.speed) bits.push(`${progress.speed.toFixed(1)}× speed`)
    return bits.length > 0 ? bits.join(" • ") : "Encoding…"
  })()

  return (
    <div className="flex flex-col gap-1">
      <Progress value={value} />
      <span className="text-xs text-muted-foreground">{detail}</span>
    </div>
  )
}

export default function Encoding() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"
  const encode = appStore((s) => s.encode)

  const [jobs, setJobs] = useState<EncodeJob[] | null>(null)
  const [paused, setPaused] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Row-level action in flight; disables that row's buttons.
  const [busyId, setBusyId] = useState<number | null>(null)

  const refresh = useCallback(async () => {
    try {
      const q = await getDaemonClient().listEncodeQueue()
      setJobs(q.jobs)
      setPaused(q.paused)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  // Re-pull on connect and whenever the queue's rev marker moves.
  useEffect(() => {
    if (connected) void refresh()
  }, [connected, encode.queueRev, refresh])

  const act = useCallback(
    async (id: number, fn: () => Promise<void>) => {
      setBusyId(id)
      try {
        await fn()
        await refresh()
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setBusyId(null)
      }
    },
    [refresh],
  )

  const togglePaused = useCallback(async () => {
    try {
      await getDaemonClient().setEncodeQueuePaused(!paused)
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [paused, refresh])

  const client = getDaemonClient()
  const canOpen = client.capabilities.openPath
  const queuedIds = (jobs ?? [])
    .filter((j) => j.status === "queued")
    .map((j) => j.id)

  return (
    <>
      <ConnectionBanner />
      <div className="mx-auto my-4 flex w-full max-w-table flex-col gap-4 px-4">
        <div className="flex items-center gap-2">
          <h2 className="text-sm font-medium">Encoding queue</h2>
          <Button
            variant="outline"
            size="sm"
            className="ml-auto h-8"
            onClick={() => void togglePaused()}
            disabled={!connected || jobs === null}
          >
            {paused ? <Play className="h-4 w-4" /> : <Pause className="h-4 w-4" />}
            {paused ? "Resume queue" : "Pause queue"}
          </Button>
        </div>

        {paused && (
          <Banner icon={Pause}>
            The queue is paused — a running job finishes, but no new jobs start
            until you resume.
          </Banner>
        )}

        {error && <p className="text-sm text-destructive">{error}</p>}

        {!connected ? (
          <p className="text-sm text-muted-foreground">Daemon not connected.</p>
        ) : jobs === null ? (
          <p className="text-sm text-muted-foreground">Loading queue…</p>
        ) : jobs.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No encoding jobs. Use "Merge parts" on a multi-part VOD in the
            Archive to queue one.
          </p>
        ) : (
          <div className="flex flex-col gap-2">
            {jobs.map((j) => {
              const busy = busyId === j.id
              const qIdx = queuedIds.indexOf(j.id)
              return (
                <Card key={j.id} className="py-0">
                  <CardContent className="flex flex-col gap-2 p-3">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-sm font-medium">
                        {jobLabel(j)}
                      </span>
                      <span className="truncate text-xs text-muted-foreground">
                        {basename(j.dir)} •{" "}
                        {j.type === "convert"
                          ? `${j.inputs[0]} → ${j.output}`
                          : `${j.inputs.length} parts → ${j.output}`}
                        {!j.delete_parts &&
                          (j.type === "convert" ? " (keeping original)" : " (keeping originals)")}
                      </span>
                      <span className="ml-auto flex shrink-0 items-center gap-1">
                        {j.warning && (
                          <Tooltip>
                            <TooltipTrigger
                              render={
                                <AlertTriangle className="h-4 w-4 text-amber-500" />
                              }
                            />
                            <TooltipContent className="max-w-sm">
                              {j.warning}
                            </TooltipContent>
                          </Tooltip>
                        )}
                        {statusBadge(j)}

                        {j.status === "done" && canOpen && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6"
                            title="Watch"
                            onClick={() =>
                              void client.openPath(joinPath(j.dir, j.output))
                            }
                          >
                            <Play className="h-4 w-4" />
                          </Button>
                        )}

                        {j.status === "queued" && (
                          <>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-6 w-6"
                              title="Move to top"
                              disabled={busy || qIdx <= 0}
                              onClick={() =>
                                void act(j.id, () =>
                                  client.reorderEncodeJob(j.id, "top"),
                                )
                              }
                            >
                              <ChevronsUp className="h-4 w-4" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-6 w-6"
                              title="Move up"
                              disabled={busy || qIdx <= 0}
                              onClick={() =>
                                void act(j.id, () =>
                                  client.reorderEncodeJob(j.id, "up"),
                                )
                              }
                            >
                              <ChevronUp className="h-4 w-4" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-6 w-6"
                              title="Move down"
                              disabled={busy || qIdx === queuedIds.length - 1}
                              onClick={() =>
                                void act(j.id, () =>
                                  client.reorderEncodeJob(j.id, "down"),
                                )
                              }
                            >
                              <ChevronDown className="h-4 w-4" />
                            </Button>
                          </>
                        )}
                        {(j.status === "queued" || j.status === "running") && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6"
                            title="Cancel"
                            disabled={busy}
                            onClick={() =>
                              void act(j.id, () => client.cancelEncodeJob(j.id))
                            }
                          >
                            <X className="h-4 w-4" />
                          </Button>
                        )}
                        {(j.status === "failed" || j.status === "cancelled") && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6"
                            title="Retry"
                            disabled={busy}
                            onClick={() =>
                              void act(j.id, () => client.retryEncodeJob(j.id))
                            }
                          >
                            <RotateCcw className="h-4 w-4" />
                          </Button>
                        )}
                        {j.status !== "running" && (
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-6 w-6"
                            title="Remove from list"
                            disabled={busy}
                            onClick={() =>
                              void act(j.id, () => client.removeEncodeJob(j.id))
                            }
                          >
                            <Trash2 className="h-4 w-4" />
                          </Button>
                        )}
                      </span>
                    </div>

                    {j.status === "running" && (
                      <RunningProgress
                        progress={
                          encode.progress?.job_id === j.id
                            ? encode.progress
                            : null
                        }
                      />
                    )}

                    {j.status === "failed" && j.error && (
                      <p className="text-xs text-destructive">
                        {j.error}
                        {j.log_path && (
                          <span className="text-muted-foreground">
                            {" "}
                            — details in {j.log_path}
                          </span>
                        )}
                      </p>
                    )}
                  </CardContent>
                </Card>
              )
            })}
          </div>
        )}
      </div>
    </>
  )
}

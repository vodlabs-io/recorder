import { useEffect, useState } from "react"
import { Loader2 } from "lucide-react"
import { Card, CardContent, CardFooter } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Progress } from "@/components/ui/progress"
import StatusIndicator from "@/components/ui/status-indicator"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { ConnectionBanner } from "@/components/connection-banner"
import { describePending } from "@/components/quit-dialog"
import { countPendingWork, hasPendingWork } from "@shared/daemon/pending-work"
import { appStore } from "@/state/app-state"
import { getDaemonClient, type UpdatePhase } from "@/lib/daemon"

// Row is one updatable component, flattened from UpdateState so the app and
// daemon halves render through exactly the same markup.
type Row = {
  key: string
  name: string
  currentVersion: string
  availableVersion: string | null
  phase: UpdatePhase
  progress: number | null
  message: string | null
  badge: string | null
}

// phaseMeta maps a phase to the indicator tier and its one-line summary. The
// dot vocabulary is the app-wide semaphore (see status-indicator.tsx); "ready"
// and "up-to-date" don't pulse because nothing is happening.
function phaseMeta(row: Row): {
  state: "ok" | "info" | "error" | "idle"
  pulse?: boolean
  label: string
} {
  switch (row.phase) {
    case "checking":
      return { state: "info", label: "Checking for updates…" }
    case "downloading":
      return {
        state: "info",
        label: `Downloading ${row.availableVersion ?? "update"}…`,
      }
    case "ready":
      return {
        state: "ok",
        pulse: false,
        label: `${row.availableVersion ?? "Update"} ready to install`,
      }
    case "up-to-date":
      return { state: "ok", pulse: false, label: "Up to date" }
    case "error":
      return { state: "error", label: "Update failed" }
    case "disabled":
      // A read-only host or a build we don't manage: the message carries the
      // detail (docker instructions, dev build, "install from the desktop app").
      return {
        state: row.availableVersion ? "info" : "idle",
        pulse: false,
        label: row.availableVersion
          ? `Version ${row.availableVersion} available`
          : "Updates are off",
      }
    case "idle":
    default:
      return { state: "idle", label: "Not checked yet" }
  }
}

function UpdateRow({ row }: { row: Row }) {
  const meta = phaseMeta(row)
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">{row.name}</span>
        <span className="font-mono text-xs text-muted-foreground">
          {row.currentVersion || "—"}
        </span>
        {/* Electrobun compares build hashes, not just version strings, so it can
            offer a rebuild of the SAME version — don't render "0.0.9 → 0.0.9". */}
        {row.availableVersion && row.availableVersion !== row.currentVersion && (
          <span className="font-mono text-xs text-muted-foreground">
            → {row.availableVersion}
          </span>
        )}
        {row.badge && (
          <Badge variant="outline" className="font-normal text-muted-foreground">
            {row.badge}
          </Badge>
        )}
      </div>
      <StatusIndicator size="sm" state={meta.state} pulse={meta.pulse} label={meta.label} />
      {row.phase === "downloading" && (
        // null renders the indeterminate bar, which is what a download with no
        // Content-Length gives us.
        <Progress className="h-1.5" value={row.progress} />
      )}
      {row.message && (
        <p
          className={
            row.phase === "error"
              ? "whitespace-pre-wrap text-xs text-destructive"
              : "whitespace-pre-wrap text-xs text-muted-foreground"
          }
        >
          {row.message}
        </p>
      )}
    </div>
  )
}

export function Update() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"
  const daemon = appStore((s) => s.daemon)
  const streamers = appStore((s) => s.streamers)
  const { app, daemon: daemonUpdate, install } = appStore((s) => s.updateState)
  const initUpdates = appStore((s) => s.initUpdates)
  const checkUpdates = appStore((s) => s.checkUpdates)
  const installUpdates = appStore((s) => s.installUpdates)

  const [confirming, setConfirming] = useState(false)
  const client = getDaemonClient()

  useEffect(() => {
    initUpdates()
  }, [initUpdates])

  // Re-check on mount so opening the tab never shows a stale answer. The daemon
  // version deps keep read-only hosts live too: they derive their rows from the
  // snapshot rather than a push.
  useEffect(() => {
    void checkUpdates()
  }, [checkUpdates, connected, daemon.version, daemon.recommendedVersion])

  const rows: Row[] = []
  if (client.capabilities.appUpdate) {
    rows.push({
      key: "app",
      name: "VodLabs Recorder",
      currentVersion: app.currentVersion,
      availableVersion: app.availableVersion,
      phase: app.phase,
      progress: app.progress,
      message: app.message,
      badge: app.channel && app.channel !== "dev" ? app.channel : null,
    })
  }
  // Until the snapshot delivers a version there is nothing to judge, so report
  // "checking" rather than trusting a phase computed from an empty version —
  // otherwise the first paint flashes a misleading "Updates are off".
  const daemonKnown = connected && !!daemonUpdate.currentVersion
  rows.push({
    key: "daemon",
    name: "Recorder service",
    currentVersion: daemonKnown ? daemonUpdate.currentVersion : "",
    availableVersion: daemonKnown ? daemonUpdate.availableVersion : null,
    phase: !connected ? "idle" : daemonKnown ? daemonUpdate.phase : "checking",
    progress: daemonUpdate.progress,
    message: !connected
      ? "Not connected."
      : daemonKnown
        ? daemonUpdate.message
        : null,
    badge: daemonUpdate.container ? "container" : null,
  })

  const busy =
    install.phase === "restarting-daemon" || install.phase === "installing-app"
  // Wait for both halves to settle before offering to install, so a pair that's
  // still downloading can't be installed twice.
  const anyReady = rows.some((r) => r.phase === "ready")
  const anySettling = rows.some(
    (r) => r.phase === "checking" || r.phase === "downloading",
  )
  const canInstall = client.capabilities.installUpdates && anyReady && !anySettling

  const pending = countPendingWork(streamers)

  function onInstallClick() {
    if (hasPendingWork(pending)) {
      setConfirming(true)
      return
    }
    void installUpdates()
  }

  function confirmInstall() {
    setConfirming(false)
    void installUpdates()
  }

  return (
    <>
      <ConnectionBanner />
      <div className="mx-auto my-4 flex w-full max-w-table flex-col gap-4 px-4">
        <p className="text-sm text-muted-foreground">
          The app and the recorder service update together. New versions download
          automatically.
        </p>

        <Card>
          <CardContent className="space-y-5">
            {rows.map((row) => (
              <UpdateRow key={row.key} row={row} />
            ))}
          </CardContent>
          {(canInstall || busy) && (
            <CardFooter className="flex-col items-end gap-2">
              <Button type="button" disabled={busy} onClick={onInstallClick}>
                {busy && <Loader2 className="h-4 w-4 animate-spin" />}
                <span>
                  {install.phase === "restarting-daemon"
                    ? "Restarting the recorder…"
                    : install.phase === "installing-app"
                      ? "Installing…"
                      : "Install and restart"}
                </span>
              </Button>
              {install.phase === "installing-app" && (
                <p className="text-xs text-muted-foreground">
                  The app will close and reopen on its own.
                </p>
              )}
            </CardFooter>
          )}
        </Card>

        {install.phase === "error" && install.message && (
          <p className="text-sm text-destructive">{install.message}</p>
        )}
      </div>

      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Install update?</AlertDialogTitle>
            <AlertDialogDescription>
              {describePending(pending)}. Installing restarts the recorder. It
              stops recording and saves what it has so far.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <Button variant="outline" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
            <Button onClick={confirmInstall}>Install</Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

export default Update

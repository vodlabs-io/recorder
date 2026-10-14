import { useEffect, useState, type ReactNode } from "react"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import {
  Check,
  ClipboardCopy,
  ExternalLink,
  FolderOpen,
  Share2,
} from "lucide-react"
import { ConnectionBanner } from "@/components/connection-banner"
import { PollingIntervals } from "@/components/polling-intervals"
import { appStore } from "@/state/app-state"
import { getDaemonClient, type DaemonInfo } from "@/lib/daemon"

// uptimeTickMs is how often the uptime figure is recomputed. It is rendered no
// finer than minutes, so a slow tick keeps it honest without re-rendering the
// screen every second.
const uptimeTickMs = 30_000

// formatUptime renders seconds as the two coarsest units that matter, so a
// daemon up for weeks doesn't print a minute count nobody reads.
function formatUptime(totalSec: number): string {
  const s = Math.max(0, Math.floor(totalSec))
  const d = Math.floor(s / 86400)
  const h = Math.floor(s / 3600) % 24
  const m = Math.floor(s / 60) % 60
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m`
  return `${s}s`
}

// Stat is one cell of the summary card. Nothing wraps: the card is a single
// fixed-height row that grows sideways as cells are added.
function Stat({ label, value }: { label: string; value: string | number }) {
  return (
    <div className="whitespace-nowrap">
      <div className="text-muted-foreground">{label}</div>
      <div className="text-2xl">{value}</div>
    </div>
  )
}

function PathRow({
  label,
  path,
  openable = true,
}: {
  label: string
  path: string
  // openable is false for a containerized daemon: the path lives inside the
  // container's filesystem, so there's nothing the host can open.
  openable?: boolean
}) {
  if (!path) return null
  // Hosts without a file manager to reveal paths in (the Web UI)
  // show the bare path; "in container" keeps its badge either way.
  const canOpen = getDaemonClient().capabilities.openPath
  return (
    <div className="grid grid-cols-[110px_1fr_auto] items-center gap-x-4 gap-y-1">
      <span className="text-right text-sm text-muted-foreground">{label}</span>
      <span className="wrap-break-word font-mono text-xs">{path}</span>
      {!openable ? (
        <Badge variant="outline" className="font-normal text-muted-foreground">
          in container
        </Badge>
      ) : canOpen ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-7"
          onClick={() => getDaemonClient().openPath(path)}
        >
          <FolderOpen className="h-4 w-4" />
          <span>Open</span>
        </Button>
      ) : (
        <span />
      )}
    </div>
  )
}
// LinkOut opens a URL in the user's own browser. The app has no business
// rendering vodlabs.io inside itself, and a link that silently does nothing is
// worse than none.
function LinkOut({ href, children }: { href: string; children: ReactNode }) {
  return (
    <Button
      variant="link"
      className="h-auto p-0 text-sm"
      onClick={() => getDaemonClient().openExternal(href)}
    >
      {children}
      <ExternalLink className="ml-1 h-3 w-3" />
    </Button>
  )
}

// logFolderOf returns the directory a log file sits in, so "Open logs folder"
// can reveal all of them at once rather than one file.
function logFolderOf(logPath: string): string {
  const cut = Math.max(logPath.lastIndexOf("/"), logPath.lastIndexOf("\\"))
  return cut > 0 ? logPath.slice(0, cut) : ""
}


export function About() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"
  const daemon = appStore((s) => s.daemon)
  const appVersion = appStore((s) => s.updateState.app.currentVersion)

  const [info, setInfo] = useState<DaemonInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  // Total archived sessions. Read as a one-session page rather than through the
  // shared listing: only the count is shown here, and the whole archive can be
  // thousands of sessions to download.
  const [vodTotal, setVodTotal] = useState<number | null>(null)
  const [copied, setCopied] = useState(false)
  const [desktopLog, setDesktopLog] = useState("")
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    if (!connected) return
    getDaemonClient()
      .getInfo()
      .then((i) => {
        setInfo(i)
        setError(null)
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
    getDaemonClient()
      .listVodsPage(0, 1)
      .then((p) => setVodTotal(p.total))
      .catch(() => setVodTotal(null))
  }, [connected])

  // The host's own log file, unlike the daemon's paths, doesn't depend on the
  // daemon connection.
  useEffect(() => {
    getDaemonClient()
      .getDesktopLogPath()
      .then(setDesktopLog)
      .catch(() => setDesktopLog(""))
  }, [])

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), uptimeTickMs)
    return () => clearInterval(id)
  }, [])

  // A bug report needs the build and the machine, and asking a user to find
  // them is how reports arrive without them. Nothing is uploaded: this opens
  // the bug report form with its versions field filled in, which the user
  // reads before posting.
  const reportBug = () => {
    const details = [
      `App: ${appVersion ?? "—"}`,
      `Daemon: ${info?.version ?? daemon.version ?? "—"}${info?.commit ? ` (${info.commit.slice(0, 12)})` : ""}`,
      `System: ${info ? `${info.os}/${info.arch}` : "—"}`,
    ].join("\n")
    getDaemonClient().openExternal(
      "https://github.com/vodlabs-io/recorder/issues/new?template=bug.yml&details=" +
        encodeURIComponent(details),
    )
  }

  const logFolder = logFolderOf(info?.daemon_log_file_path ?? "")

  const uptime =
    connected && daemon.startTime !== null
      ? formatUptime(Math.floor(now / 1000) - daemon.startTime)
      : "—"

  // Everything a maintainer asks for after "what happened", in one paste. It
  // is built from what is already on this screen, so a user can read it before
  // sending it — and nothing leaves the machine on its own.
  const copyDiagnostics = async () => {
    const lines = [
      `App:      ${appVersion ?? "—"}`,
      `Daemon:   ${info?.version ?? daemon.version ?? "—"}${info?.commit ? ` (${info.commit})` : ""}`,
      `System:   ${info ? `${info.os}/${info.arch}` : "—"}`,
      `Service:  ${info?.cloud_api_url ?? "—"}`,
      `Reachable: ${daemon.service ? (daemon.service.reachable ? "yes" : `no, since ${daemon.service.since}`) : "—"}`,
      `Streamers: ${daemon.subscriptionCount}, recording ${daemon.workerCount}`,
      `Uptime:   ${uptime}`,
      `Logs:     ${info?.daemon_log_file_path ?? "—"}`,
    ]
    try {
      await navigator.clipboard.writeText(lines.join("\n"))
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setCopied(false)
    }
  }

  return (
    <>
      <ConnectionBanner />
      <div className="mx-auto my-4 flex w-full max-w-table flex-col gap-4 px-4">
        <Card>
          <CardContent>
            {/* One row, however many cells: the card keeps its height and
                scrolls sideways rather than clipping on a narrow window. */}
            <div className="flex gap-6 overflow-x-auto">
              <Stat label="Subscribed" value={daemon.subscriptionCount} />
              <Stat label="Recording" value={daemon.workerCount} />
              <Stat label="VODs" value={vodTotal ?? "—"} />
              {/* The Web UI has no build of its own to report. */}
              {appVersion && <Stat label="App version" value={appVersion} />}
              <Stat
                label="Daemon version"
                value={info?.version || daemon.version || "—"}
              />
              <Stat label="Daemon uptime" value={uptime} />
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap gap-x-6 gap-y-2 text-sm">
              <LinkOut href="https://vodlabs.io">Website</LinkOut>
              <LinkOut href="https://vodlabs.io/docs">Documentation</LinkOut>
              <LinkOut href="https://github.com/vodlabs-io/recorder">Source code</LinkOut>
              <LinkOut href="https://vodlabs.io/docs/privacy">Privacy</LinkOut>
              <LinkOut href="https://github.com/vodlabs-io/recorder/blob/main/LICENSE">
                License
              </LinkOut>
              <LinkOut href="https://github.com/vodlabs-io/recorder/blob/main/THIRD_PARTY_NOTICES.md">
                Third-party notices
              </LinkOut>
            </div>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="outline" onClick={reportBug}>
                Report a bug
                <ExternalLink className="ml-1 h-3 w-3" />
              </Button>
              <Button size="sm" variant="outline" onClick={copyDiagnostics}>
                {copied ? (
                  <>
                    <Check className="mr-1 h-3 w-3" />
                    Copied
                  </>
                ) : (
                  <>
                    <ClipboardCopy className="mr-1 h-3 w-3" />
                    Copy diagnostics
                  </>
                )}
              </Button>
              {logFolder && getDaemonClient().capabilities.openPath && (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => getDaemonClient().openPath(logFolder)}
                >
                  <FolderOpen className="mr-1 h-3 w-3" />
                  Open logs folder
                </Button>
              )}
            </div>
            <p className="text-xs text-muted-foreground">
              VodLabs Recorder is source-available software. It is not affiliated
              with, or endorsed by, any of the platforms it records from.
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardContent>
            <PollingIntervals />
          </CardContent>
        </Card>
        <Card>
          <CardContent>
            {!info ? (
              <p className="text-sm text-muted-foreground">
                {connected ? "Loading daemon info…" : "Daemon not connected."}
                {error ? ` ${error}` : ""}
              </p>
            ) : (
              <div>
                <div className="space-y-2">
                  <div className="grid grid-cols-[110px_1fr_auto] items-center gap-x-4">
                    <span className="text-right text-sm text-muted-foreground">
                      Daemon URL
                    </span>
                    <span className="font-mono text-xs">{info.nats_url}</span>
                    <span />
                  </div>

                  <PathRow
                    label="Download folder"
                    path={info.download_dir}
                    openable={!info.container}
                  />
                  {/* Empty when archiving is off, which PathRow renders as nothing. */}
                  <PathRow
                    label="Archive folder"
                    path={info.archive_dir}
                    openable={!info.container}
                  />
                  <PathRow
                    label="Config file"
                    path={info.config_file_path}
                    openable={!info.container}
                  />
                  <PathRow
                    label="Streamers file"
                    path={info.streamers_file_path}
                    openable={!info.container}
                  />
                  {/* In a container the daemon logs to stdout (logger.mode=console),
                      not to files, so there's nothing on the host to open. */}
                  {!info.container && (
                    <>
                      <PathRow
                        label="Daemon log"
                        path={info.daemon_log_file_path}
                      />
                      <PathRow
                        label="Session log"
                        path={info.session_log_file_path}
                      />
                      <PathRow
                        label="Processing log"
                        path={info.processing_log_file_path}
                      />
                      <PathRow
                        label="Cloud log"
                        path={info.cloud_log_file_path}
                      />
                      <PathRow
                        label="Scraper log"
                        path={info.scraper_log_file_path}
                      />
                    </>
                  )}
                  {/* The app's own log, written wherever the app runs — including
                      alongside a containerized daemon. */}
                  <PathRow label="Desktop log" path={desktopLog} />
                  {info.container && (

                    <p className="pt-1 text-xs text-muted-foreground">
                      The listed paths are inside the Docker container.
                      Host paths can be changed by editing your docker-compose configuration. View logs with{" "}
                      <span className="font-mono">docker compose logs -f</span>.
                    </p>


                  )}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  )
}

export default About

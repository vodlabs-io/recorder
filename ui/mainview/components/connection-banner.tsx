import { AlertTriangle, Loader2 } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import { Button } from "@/components/ui/button"
import { appStore } from "@/state/app-state"
import type { ConnectionState } from "@/lib/daemon"

function labelFor(state: ConnectionState): string {
  switch (state) {
    case "connecting":
      return "Connecting to the recorder…"
    case "downloading":
      return "Setting up…"
    case "disconnected":
      return "Recorder disconnected"
    case "no-binary":
      return "Recorder unavailable"
    default:
      return "Connected"
  }
}

export function ConnectionBanner() {
  const connection = appStore((s) => s.connection)
  const retryConnect = appStore((s) => s.retryConnect)

  if (connection.state === "connected") return null
  // "downloading" is a first-run setup step, not an error: show a spinner and no
  // Retry, same as "connecting".
  const connecting =
    connection.state === "connecting" || connection.state === "downloading"

  return (
    <Banner
      className={PAGE_BANNER}
      icon={connecting ? Loader2 : AlertTriangle}
      iconClassName={connecting ? "animate-spin" : undefined}
      action={
        connecting ? undefined : (
          <Button size="sm" variant="outline" onClick={() => retryConnect()}>
            Retry
          </Button>
        )
      }
    >
      <span className="font-medium">{labelFor(connection.state)}</span>
      {connection.message ? (
        <span className="text-muted-foreground"> {connection.message}</span>
      ) : null}
    </Banner>
  )
}

export default ConnectionBanner

import { useEffect, useState } from "react"
import { Bug, ExternalLink } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import { Button } from "@/components/ui/button"
import { getDaemonClient } from "@/lib/daemon"
import { refreshErrorReports, setErrorReports, useErrorReports } from "@/lib/error-reports"
import { appStore } from "@/state/app-state"

const PRIVACY_URL = "https://vodlabs.io/privacy#error-reports"

// PrivacyLink opens the policy's section on what error and bug reports hold.
export function PrivacyLink({ children = "What's in a report" }: { children?: string }) {
  return (
    <Button
      variant="link"
      className="h-auto p-0 text-xs"
      onClick={() => getDaemonClient().openExternal(PRIVACY_URL)}
    >
      {children}
      <ExternalLink className="ml-1 h-3 w-3" />
    </Button>
  )
}

// ErrorReportsBanner asks, once, whether to send error reports. A new install
// answers in the welcome dialog; this is for one that predates the question.
// Either answer hides it for good: Settings › General changes it later.
export function ErrorReportsBanner() {
  const reports = useErrorReports()
  const connected = appStore((s) => s.connection.state === "connected")
  const welcomeSeen = appStore((s) => s.prefsHydrated && s.prefs.welcome_seen)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (connected) void refreshErrorReports()
  }, [connected])

  if (!connected || !welcomeSeen || !reports?.available || reports.enabled !== null) return null

  const answer = async (enabled: boolean) => {
    setBusy(true)
    try {
      await setErrorReports(enabled)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Banner
      className={PAGE_BANNER}
      tone="info"
      icon={Bug}
      action={
        <div className="flex shrink-0 gap-2">
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => answer(false)}>
            No thanks
          </Button>
          <Button size="sm" disabled={busy} onClick={() => answer(true)}>
            Send reports
          </Button>
        </div>
      }
    >
      <span className="font-medium">Help fix bugs by sending error reports?</span>
      <span className="text-muted-foreground">
        {" "}
        When the recorder hits an error or crashes, it tells VodLabs what went wrong. Reports
        can name streamers and folders, never your logins. You can change this in Settings.{" "}
      </span>
      <PrivacyLink />
    </Banner>
  )
}

export default ErrorReportsBanner

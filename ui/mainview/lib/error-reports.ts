// The user's answer to sending error reports, as the daemon holds it, and the
// UI's own error reports, which follow it. Settings, the welcome dialog and the
// banner that asks read and change it through here, so each sees the others'
// change at once.

import { useSyncExternalStore } from "react"
import * as Sentry from "@sentry/react"
import type { ErrorEvent } from "@sentry/react"
import { scrubBreadcrumb, scrubEvent } from "@shared/report-scrub"
import { getDaemonClient, type ErrorReportsState } from "@/lib/daemon"
import { appStore } from "@/state/app-state"

// The recorder project's public client key, as in internal/errreport.
const DSN =
  "https://748fd4caebaf0ca976ef37c2c4b4ddc9@o4512343562780672.ingest.us.sentry.io/4512344170758144"

let state: ErrorReportsState | null = null
const listeners = new Set<() => void>()
let sentryStarted = false

function publish(next: ErrorReportsState) {
  state = next
  followAnswer()
  listeners.forEach((l) => l())
}

// refreshErrorReports re-reads the answer, for a screen that just mounted or a
// daemon that just connected.
export async function refreshErrorReports(): Promise<void> {
  try {
    publish(await getDaemonClient().getErrorReports())
  } catch {
    /* the daemon is not reachable; keep what we had */
  }
}

export async function setErrorReports(enabled: boolean): Promise<void> {
  publish(await getDaemonClient().setErrorReports(enabled))
}

export function useErrorReports(): ErrorReportsState | null {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l)
      return () => listeners.delete(l)
    },
    () => state,
  )
}

// followAnswer starts the UI's Sentry client the first time reports are on.
// Turning them off later leaves it running but sending nothing.
function followAnswer() {
  if (sentryStarted || state?.enabled !== true || !state.available) return
  sentryStarted = true
  const { appVersion, web } = buildInfo()
  Sentry.init({
    dsn: DSN,
    release: `recorder@${appVersion}`,
    // A Web UI served by a development daemon is a development build too.
    environment:
      import.meta.env.DEV || appVersion === "devel" ? "development" : "production",
    dataCollection: {
      userInfo: true, // lets Sentry record the IP address
      cookies: false,
      // User-Agent gives the browser and OS; Referer could carry a query.
      httpHeaders: { request: { allow: ["User-Agent"] }, response: false },
      httpBodies: [],
      urlQueryParams: false,
    },
    ignoreErrors: ["ResizeObserver loop"],
    // Session tracking would keep reporting after the user turns reports off.
    integrations: (defaults) => defaults.filter((i) => i.name !== "BrowserSession"),
    initialScope: { tags: { component: web ? "ui-web" : "ui-desktop" } },
    beforeSend: (ev: ErrorEvent) => (state?.enabled === true ? scrubEvent(ev) : null),
    beforeBreadcrumb: (b) => scrubBreadcrumb(b),
  })
}

// buildInfo names the build the UI belongs to: the desktop app's version, or
// for the Web UI, which has none of its own, the daemon's.
function buildInfo(): { appVersion: string; web: boolean } {
  const web = !getDaemonClient().capabilities.installUpdates
  const s = appStore.getState()
  const version = web ? s.daemon.version : s.updateState.app.currentVersion
  return { appVersion: version || "unknown", web }
}

// For createRoot's onUncaughtError / onCaughtError / onRecoverableError; a
// no-op until reports are on.
export function reactErrorHandler() {
  const handler = Sentry.reactErrorHandler()
  return (error: unknown, info: { componentStack?: string }) => {
    if (sentryStarted && state?.enabled === true) handler(error, info)
  }
}

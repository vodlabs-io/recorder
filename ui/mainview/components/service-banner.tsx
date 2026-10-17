import { CloudOff, ListX } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import { appStore } from "@/state/app-state"
import type { ServiceState } from "@shared/types"

// ServiceBanner explains a daemon that cannot reach the VodLabs service, or
// whose streamer list the service has refused.
//
// Whether a streamer is live is the service's answer, so while it is
// unreachable nothing is checked and no new recording can start — recordings
// already running carry on. Without this the app just shows everyone as they
// were last seen, which reads as a quiet evening rather than an outage.
export function ServiceBanner() {
  const service = appStore((s) => s.daemon.service)
  const registration = appStore((s) => s.daemon.registration)

  // null until the daemon has written it; absence is not a problem. A daemon
  // that has never registered has its own banner, which says more.
  if (!service) return null
  if (registration && !registration.registered) return null
  if (service.reachable) {
    return service.subscribe_error ? <SubscribeBanner service={service} /> : null
  }

  return (
    <Banner className={PAGE_BANNER} icon={CloudOff} align="start">
      <span className="font-medium">Can't reach the VodLabs service</span>
      <span className="text-muted-foreground">
        {" "}
        It keeps retrying. Recordings already running continue, but nothing can
        be checked and no new recording starts until it's back.
      </span>
      {service.error ? (
        <span className="mt-1 block text-xs text-muted-foreground">
          Last error: {service.error}
        </span>
      ) : null}
    </Banner>
  )
}

// SubscribeBanner explains a streamer list the service has not accepted yet.
// Rows added since the last good subscribe would otherwise read "Monitoring"
// while nothing checks them.
function SubscribeBanner({ service }: { service: ServiceState }) {
  const retryAt = service.subscribe_retry_at ? new Date(service.subscribe_retry_at) : null
  const retry =
    retryAt && retryAt.getTime() > Date.now()
      ? ` It retries at ${retryAt.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}.`
      : " It keeps retrying."
  return (
    <Banner className={PAGE_BANNER} icon={ListX} align="start">
      <span className="font-medium">
        {service.rate_limited
          ? "Streamer list changed too often"
          : "Couldn't update your streamers with the VodLabs service"}
      </span>
      <span className="text-muted-foreground">
        {" "}
        Streamers added since the last update aren't checked yet, so they can't
        start recording.{retry}
      </span>
      <span className="mt-1 block text-xs text-muted-foreground">
        Last error: {service.subscribe_error}
      </span>
    </Banner>
  )
}

export default ServiceBanner

import { CloudOff } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import { appStore } from "@/state/app-state"

// ServiceBanner explains a daemon that cannot reach the VodLabs service.
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
  if (!service || service.reachable) return null
  if (registration && !registration.registered) return null

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

export default ServiceBanner

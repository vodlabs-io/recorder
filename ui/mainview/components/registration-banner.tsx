import { AlertTriangle } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import { appStore } from "@/state/app-state"

// RegistrationBanner explains a daemon that came up without an identity.
//
// A daemon registers itself with the VodLabs service on its very first
// start, and until that succeeds it cannot look up whether anyone is live. It
// keeps running and retrying, so the failure is otherwise invisible:
// streamers can be added and simply never report a status, which reads as a
// broken install rather than a machine that could not reach the network on
// its first run.
//
// Only ever shown before a first successful registration — the credentials are
// on disk afterwards, and a later outage is an ordinary connection problem.
export function RegistrationBanner() {
  const registration = appStore((s) => s.daemon.registration)

  // null until the daemon has written it; absence is not a problem.
  if (!registration || registration.registered) return null

  return (
    <Banner className={PAGE_BANNER} icon={AlertTriangle}>
      <span className="font-medium">
        This recorder could not reach the VodLabs service to set itself up
      </span>
      <span className="text-muted-foreground">
        {" "}
        It keeps retrying in the background, and everything here starts working
        once it succeeds. Until then it cannot tell whether anyone is live.
        Check this machine's internet connection.
      </span>
      {registration.error ? (
        <span className="mt-1 block text-xs text-muted-foreground">
          Last error: {registration.error}
        </span>
      ) : null}
    </Banner>
  )
}

export default RegistrationBanner

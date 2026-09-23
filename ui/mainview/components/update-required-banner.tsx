import { CircleArrowUp } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import { Button } from "@/components/ui/button"
import { appStore } from "@/state/app-state"

// UpdateRequiredBanner explains a daemon the service has stopped serving.
//
// Below the minimum supported version the service refuses everything but the
// heartbeat, so nothing is checked and nothing records — while the app itself
// keeps working, which reads as a broken install rather than a version that is
// simply too old. The heartbeat is answered precisely so this can be said.
export function UpdateRequiredBanner() {
  const updateRequired = appStore((s) => s.daemon.updateRequired)
  const recommended = appStore((s) => s.daemon.recommendedVersion)

  if (!updateRequired) return null

  return (
    <Banner tone="danger" className={PAGE_BANNER} icon={CircleArrowUp} align="start"
      action={
        <Button size="sm" variant="outline" onClick={() => { window.location.hash = "#/update" }}>
          Update
        </Button>
      }
    >
      <span className="font-medium">This recorder is too old to use the VodLabs service</span>
      <span className="text-muted-foreground">
        {" "}
        Nothing can be checked or recorded until it updates
        {recommended ? ` to ${recommended}` : ""}.
      </span>
    </Banner>
  )
}

export default UpdateRequiredBanner

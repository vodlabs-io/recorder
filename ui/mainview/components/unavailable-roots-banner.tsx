import { AlertTriangle } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"

// UnavailableRootsBanner reports Storage roots the archive scan could not walk.
// Unlike LowSpaceBanner this is driven by the scan result rather than the
// daemon's disk samples, so it takes the roots as a prop.
export function UnavailableRootsBanner({ roots }: { roots: string[] }) {
  if (roots.length === 0) return null

  return (
    <Banner className={PAGE_BANNER} icon={AlertTriangle}>
      {roots.length === 1
        ? `1 storage root is unavailable: ${roots[0]}`
        : `${roots.length} storage roots are unavailable (offline drive?).`}
    </Banner>
  )
}

export default UnavailableRootsBanner

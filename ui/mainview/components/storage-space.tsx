import { AlertTriangle, HardDrive, HelpCircle } from "lucide-react"
import { Banner, PAGE_BANNER } from "@/components/banner"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { appStore } from "@/state/app-state"
import type { DiskStatus, SpaceState, StorageRoot } from "@/lib/daemon"
import { formatBytes } from "@/lib/utils"

// Storage space, reported per Storage root — the distinct filesystem behind one
// or more download or archive folders. Folders that share a disk are collapsed
// by the daemon, so two bars never show the same numbers.

function usedFraction(root: StorageRoot): number {
  const { total_bytes: total, free_bytes: free } = root.usage
  if (total <= 0) return 0
  return Math.min(1, Math.max(0, (total - free) / total))
}

function barClass(state: SpaceState): string {
  switch (state) {
    case "full":
      return "bg-red-500"
    case "low":
      return "bg-amber-500"
    default:
      return "bg-muted-foreground/60"
  }
}

function rootLabel(root: StorageRoot): string {
  return root.paths[0] ?? "(unset)"
}

// StorageBars renders one bar per Storage root. Returns null until the daemon
// has published a sample, so a cold start shows nothing rather than zeros.
export function StorageBars({ className }: { className?: string }) {
  const diskStatus = appStore((s) => s.daemon.diskStatus)
  if (!diskStatus || diskStatus.roots.length === 0) return null

  return (
    <div className={className}>
      {diskStatus.roots.map((root) => (
        <StorageBar key={rootLabel(root)} root={root} />
      ))}
    </div>
  )
}

function StorageBar({ root }: { root: StorageRoot }) {
  const label = rootLabel(root)
  const extra = root.paths.length - 1

  // "unknown" is not zero free space — the root could not be probed at all.
  // Showing an empty bar would read as a full disk, which is a different
  // problem with a different fix.
  if (root.state === "unknown") {
    return (
      <div className="flex items-center gap-2 py-1 text-sm">
        <HelpCircle className="h-4 w-4 shrink-0 text-muted-foreground" />
        <span className="truncate" title={label}>
          {label}
        </span>
        <span className="text-muted-foreground">unavailable</span>
      </div>
    )
  }

  const used = usedFraction(root)
  return (
    <div className="py-1 text-sm">
      <div className="flex items-center gap-2">
        <HardDrive className="h-4 w-4 shrink-0 text-muted-foreground" />
        <span className="truncate" title={root.paths.join("\n")}>
          {label}
        </span>
        {extra > 0 && (
          <span className="text-muted-foreground">
            +{extra} more folder{extra > 1 ? "s" : ""}
          </span>
        )}
        <span className="ml-auto shrink-0 tabular-nums text-muted-foreground">
          {formatBytes(root.usage.free_bytes)} free of{" "}
          {formatBytes(root.usage.total_bytes)}
        </span>
      </div>
      <div
        className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(used * 100)}
        aria-label={`${label} disk usage`}
      >
        <div
          className={`h-full ${barClass(root.state)}`}
          style={{ width: `${used * 100}%` }}
        />
      </div>
    </div>
  )
}

// StorageAttention is how loudly the sidebar should point at Settings → Files.
// "critical" is a root that has actually run out; "warn" covers a root running
// low and one that could not be probed, which is worth a look for a different
// reason.
export type StorageAttention = "none" | "warn" | "critical"

// selectStorageAttention is the one definition of "storage needs attention",
// shared by the sidebar dot and the banner so the two cannot disagree about
// whether there is a problem.
export function selectStorageAttention(s: {
  daemon: { diskStatus: DiskStatus | null }
}): StorageAttention {
  const roots = s.daemon.diskStatus?.roots
  if (!roots || roots.length === 0) return "none"
  if (roots.some((r) => r.state === "full")) return "critical"
  if (roots.some((r) => r.state === "low" || r.state === "unknown")) return "warn"
  return "none"
}

// StorageCard is StorageBars in its own card, for the Settings screens. It
// renders nothing until the daemon has published a sample, so a cold start
// leaves no empty card behind.
export function StorageCard() {
  const diskStatus = appStore((s) => s.daemon.diskStatus)
  if (!diskStatus || diskStatus.roots.length === 0) return null

  return (
    <Card className="w-full">
      <CardHeader>
        <CardTitle className="text-sm">Disk space</CardTitle>
      </CardHeader>
      <CardContent>
        <StorageBars />
      </CardContent>
    </Card>
  )
}

// LowSpaceBanner warns about Storage roots that are low, full or unreachable.
//
// The condition belongs to the folder, not to any one VOD: twenty wedged VODs
// are one problem, so this is shown once rather than repeated on every row.
export function LowSpaceBanner() {
  const diskStatus = appStore((s) => s.daemon.diskStatus)
  if (!diskStatus) return null

  const full = diskStatus.roots.filter((r) => r.state === "full")
  const low = diskStatus.roots.filter((r) => r.state === "low")
  if (full.length === 0 && low.length === 0) return null

  return (
    // A full root needs a different action, so it gets a louder tone and its
    // own message rather than one generic warning.
    <Banner
      className={PAGE_BANNER}
      tone={full.length > 0 ? "danger" : "warning"}
      icon={AlertTriangle}
      align="start"
    >
      <div className="space-y-0.5">
        {full.map((r) => (
          <div key={rootLabel(r)}>
            <span className="font-medium">Out of space:</span>{" "}
            <span className="break-all">{rootLabel(r)}</span> has only{" "}
            {formatBytes(r.usage.free_bytes)} free. Recordings will not start and
            finished ones cannot be moved until you free some space.
          </div>
        ))}
        {low.map((r) => (
          <div key={rootLabel(r)}>
            <span className="font-medium">Running low:</span>{" "}
            <span className="break-all">{rootLabel(r)}</span> has{" "}
            {formatBytes(r.usage.free_bytes)} free.
          </div>
        ))}
      </div>
    </Banner>
  )
}

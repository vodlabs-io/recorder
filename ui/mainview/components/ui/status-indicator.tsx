import * as React from "react"
import { CircleAlert } from "lucide-react"
import { cn } from "@/lib/utils"

export type IndicatorState = "ok" | "info" | "degraded" | "error" | "idle"

// StatusIndicator renders a small colored dot (or, for "error", an alert
// glyph), optionally pulsing, to convey a streamer's health at a glance.
//
// The tiers form a semaphore — color carries severity, never activity type:
//   ok       green   working as intended (recording)
//   info     blue    busy and normal (monitoring, processing, moving)
//   degraded yellow  needs attention but may self-heal (lagging, restricted)
//   error    red     needs the user to act (invalid config) — rendered as a
//                    stroke CircleAlert, not a dot, so the app's filled red
//                    dot stays reserved for the REC badge's recording motif
//   idle     grey    intentionally inactive
//
// Pulsing defaults to the states that represent ongoing activity; pass
// `pulse` to override (e.g. a persistent "restricted" renders degraded but
// solid, since nothing is actively happening).
interface StatusIndicatorProps {
  state: IndicatorState
  pulse?: boolean
  label?: string
  className?: string
  size?: "sm" | "md" | "lg"
  labelClassName?: string
}

// STATUS_BADGE_CLASSES tints a status badge to the tier's semaphore color, for
// the places that spell a state out instead of showing a dot. It lives beside
// the dot colors so the two renderings of one tier can't drift apart.
export const STATUS_BADGE_CLASSES: Record<IndicatorState, string> = {
  ok: "bg-green-50 text-green-700 dark:bg-green-950 dark:text-green-300",
  info: "bg-blue-50 text-blue-700 dark:bg-blue-950 dark:text-blue-300",
  degraded:
    "bg-yellow-50 text-yellow-700 dark:bg-yellow-950 dark:text-yellow-300",
  error: "bg-red-50 text-red-700 dark:bg-red-950 dark:text-red-300",
  idle: "bg-slate-50 text-slate-700 dark:bg-slate-900 dark:text-slate-300",
}

const getStateColors = (state: StatusIndicatorProps["state"]) => {
  switch (state) {
    case "ok":
      return { dot: "bg-green-500", ping: "bg-green-300" }
    case "info":
      return { dot: "bg-blue-500", ping: "bg-blue-300" }
    case "degraded":
      return { dot: "bg-yellow-500", ping: "bg-yellow-300" }
    case "error":
      // Unused (error renders an icon), but keeps the map total.
      return { dot: "bg-destructive", ping: "bg-destructive" }
    case "idle":
    default:
      return { dot: "bg-slate-500", ping: "bg-slate-400" }
  }
}

const getSizeClasses = (size: StatusIndicatorProps["size"]) => {
  switch (size) {
    case "sm":
      return { dot: "h-2 w-2", ping: "h-2 w-2", icon: "h-3.5 w-3.5" }
    case "lg":
      return { dot: "h-4 w-4", ping: "h-4 w-4", icon: "h-5 w-5" }
    case "md":
    default:
      return { dot: "h-3 w-3", ping: "h-3 w-3", icon: "h-4 w-4" }
  }
}

function StatusIndicator({
  state = "idle",
  pulse,
  label,
  className,
  size = "md",
  labelClassName,
}: StatusIndicatorProps) {
  const shouldAnimate = pulse ?? (state === "ok" || state === "info" || state === "degraded")
  const colors = getStateColors(state)
  const sizeClasses = getSizeClasses(size)

  return (
    <div className={cn("flex items-center gap-2", className)}>
      <div className="relative flex items-center">
        {state === "error" ? (
          // A stroke icon, deliberately not a filled dot: shape says "error,
          // not activity", and it can't be confused with the dot tiers.
          <CircleAlert
            className={cn("text-destructive", sizeClasses.icon)}
            // The dot tiers are color-only; give the icon tier its lucide
            // stroke so it reads at table size.
            strokeWidth={2.5}
          />
        ) : (
          <>
            {shouldAnimate && (
              <span
                className={cn(
                  "absolute inline-flex rounded-full opacity-75 animate-ping",
                  sizeClasses.ping,
                  colors.ping,
                )}
              />
            )}
            <span
              className={cn(
                "relative inline-flex rounded-full",
                sizeClasses.dot,
                colors.dot,
              )}
            />
          </>
        )}
      </div>
      {label && (
        <p className={cn("text-sm text-muted-foreground", labelClassName)}>
          {label}
        </p>
      )}
    </div>
  )
}

export default StatusIndicator

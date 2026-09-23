import type { ComponentType, ReactNode } from "react"
import { cn } from "@/lib/utils"

// One notice strip, shared by every banner in the app so tone, spacing and
// icon treatment cannot drift apart between screens.

export type BannerTone = "warning" | "danger"

const TONE: Record<BannerTone, { box: string; icon: string }> = {
  warning: { box: "border-amber-500/40 bg-amber-500/10", icon: "text-amber-500" },
  danger: { box: "border-red-500/40 bg-red-500/10", icon: "text-red-500" },
}

// Banners that head a screen carry their own margin, because they sit outside
// the screen's content column.
export const PAGE_BANNER = "mx-4 mt-4"

export function Banner({
  tone = "warning",
  icon: Icon,
  iconClassName,
  align = "center",
  action,
  className,
  children,
}: {
  tone?: BannerTone
  icon: ComponentType<{ className?: string }>
  /** Extra icon classes, e.g. `animate-spin`. */
  iconClassName?: string
  /** `start` for multi-line bodies, so the icon tracks the first line. */
  align?: "center" | "start"
  action?: ReactNode
  className?: string
  children: ReactNode
}) {
  const tones = TONE[tone]
  return (
    <div
      className={cn(
        "flex gap-3 rounded-md border px-4 py-2 text-sm",
        align === "start" ? "items-start" : "items-center",
        tones.box,
        className,
      )}
    >
      <Icon
        className={cn(
          "h-4 w-4 shrink-0",
          // text-sm leads 20px, so 2px down centres the icon on the first line.
          align === "start" && "mt-0.5",
          tones.icon,
          iconClassName,
        )}
      />
      <div className="flex-1">{children}</div>
      {action}
    </div>
  )
}

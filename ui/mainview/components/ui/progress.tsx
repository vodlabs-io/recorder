"use client"

import { Progress as ProgressPrimitive } from "@base-ui/react/progress"

import { cn } from "@/lib/utils"

// Progress is a thin themed wrapper over the base-ui progress primitive.
// value is 0-100; pass null for an indeterminate bar (pulsing full track).
function Progress({
  className,
  value,
  ...props
}: ProgressPrimitive.Root.Props) {
  return (
    <ProgressPrimitive.Root data-slot="progress" value={value} {...props}>
      <ProgressPrimitive.Track
        data-slot="progress-track"
        className={cn(
          "block h-2 w-full overflow-hidden rounded-full bg-primary/20",
          className,
        )}
      >
        <ProgressPrimitive.Indicator
          data-slot="progress-indicator"
          className="block h-full rounded-full bg-primary transition-[width] duration-300 data-[indeterminate]:w-full data-[indeterminate]:animate-pulse"
        />
      </ProgressPrimitive.Track>
    </ProgressPrimitive.Root>
  )
}

export { Progress }

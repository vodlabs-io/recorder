import type { ReactNode } from "react"
import { Label } from "@/components/ui/label"
import { cn } from "@/lib/utils"

// Shared two-column layout for every Settings screen: a right-justified label in
// a fixed-width gutter, then the control (and any hints/extra rows) in the second
// column. Centralizing the grid template here keeps labels and controls lined up
// identically across all Settings screens — existing and future ones just render
// their fields as <SettingsRow>s and inherit the layout.
//
// `align` sets how the label meets the control: "center" (default, for
// single-line controls) or "start" (top-aligned, for taller/multi-line ones).
export function SettingsRow({
  label,
  control,
  align = "center",
  children,
}: {
  label: ReactNode
  control: ReactNode
  align?: "center" | "start"
  // Hints or extra controls stacked in the second column, below the control.
  children?: ReactNode
}) {
  return (
    <div
      className={cn(
        "grid grid-cols-[10rem_minmax(0,460px)] gap-x-4 gap-y-2",
        align === "start" ? "items-start" : "items-center",
      )}
    >
      <Label
        className={cn("justify-end text-right", align === "start" && "pt-1.5")}
      >
        {label}
      </Label>
      <div className="min-w-0">{control}</div>
      {children != null && (
        <>
          {/* Empty label-column cell so the second-column extras line up with
              the control above them. */}
          <div aria-hidden />
          <div className="min-w-0 space-y-2">{children}</div>
        </>
      )}
    </div>
  )
}

// Muted description/help text for a SettingsRow's second column. A <div> (not a
// <p>) so it can wrap block content such as a monospace variable list.
export function SettingsHint({
  className,
  children,
}: {
  className?: string
  children: ReactNode
}) {
  return (
    <div className={cn("text-sm text-muted-foreground", className)}>
      {children}
    </div>
  )
}

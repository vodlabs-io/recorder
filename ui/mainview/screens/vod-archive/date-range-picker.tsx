import { useMemo, useState } from "react"
import { DayPicker, type DateRange } from "react-day-picker"
import "react-day-picker/style.css"
import { CalendarRange, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { cn } from "@/lib/utils"
import { formatDateValue, parseLocalDate } from "./format"

// DateRangePicker filters the VOD Archive by recorded date. The empty value means
// "no filter" and displays as "All time" — deliberately NOT today's date or
// the VOD Archive's min/max: a populated default would be a real filter value
// that silently excludes anything recorded after it.
//
// Values are "YYYY-MM-DD" strings ("" = unset).

type Props = {
  from: string
  to: string
  onChange: (from: string, to: string) => void
  // Archive bounds: days outside [min, max] hold no VODs and are disabled.
  min?: Date
  max?: Date
}

function formatDay(d: Date, withYear: boolean): string {
  return d.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    ...(withYear ? { year: "numeric" } : {}),
  })
}

// triggerLabel renders the active filter compactly: "All time", a single day,
// "Mar 3 – Jul 15, 2026", or a half-open "From/Until …".
function triggerLabel(from: Date | null, to: Date | null): string {
  if (!from && !to) return "All time"
  const thisYear = new Date().getFullYear()
  if (from && to) {
    const sameYear = from.getFullYear() === to.getFullYear()
    if (from.getTime() === to.getTime()) {
      return formatDay(from, from.getFullYear() !== thisYear)
    }
    return `${formatDay(from, !sameYear)} – ${formatDay(to, to.getFullYear() !== thisYear || !sameYear)}`
  }
  const d = (from ?? to) as Date
  return `${from ? "From" : "Until"} ${formatDay(d, d.getFullYear() !== thisYear)}`
}

export function DateRangePicker({ from, to, onChange, min, max }: Props) {
  const [open, setOpen] = useState(false)

  const fromDate = useMemo(() => (from ? parseLocalDate(from) : null), [from])
  const toDate = useMemo(() => (to ? parseLocalDate(to) : null), [to])
  const active = fromDate !== null || toDate !== null

  const selected: DateRange | undefined = active
    ? { from: fromDate ?? undefined, to: toDate ?? undefined }
    : undefined

  const onSelect = (range: DateRange | undefined) => {
    onChange(
      range?.from ? formatDateValue(range.from) : "",
      range?.to ? formatDateValue(range.to) : "",
    )
  }

  const today = new Date()
  const applyPreset = (f: Date | null, t: Date | null) => {
    onChange(f ? formatDateValue(f) : "", t ? formatDateValue(t) : "")
    setOpen(false)
  }
  const daysAgo = (n: number) => {
    const d = new Date(today.getFullYear(), today.getMonth(), today.getDate())
    d.setDate(d.getDate() - n)
    return d
  }
  const presets: Array<{ label: string; from: Date | null; to: Date | null; active: boolean }> = [
    { label: "All time", from: null, to: null, active: !active },
    { label: "Last 7 days", from: daysAgo(6), to: today, active: false },
    { label: "Last 30 days", from: daysAgo(29), to: today, active: false },
    {
      label: "This year",
      from: new Date(today.getFullYear(), 0, 1),
      to: today,
      active: false,
    },
  ]

  // Open on the month that ends the current selection (or the VOD Archive's
  // newest month), with the previous month alongside.
  const endMonth = toDate ?? max ?? today
  const defaultMonth = new Date(endMonth.getFullYear(), endMonth.getMonth() - 1, 1)

  return (
    <div className="flex items-center">
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          render={
            <Button
              variant="outline"
              size="sm"
              className={cn("h-8 font-normal", !active && "text-muted-foreground")}
            >
              <CalendarRange className="h-4 w-4" />
              {triggerLabel(fromDate, toDate)}
            </Button>
          }
        />
        <PopoverContent align="start" className="w-auto p-3">
          <div className="flex flex-col gap-3 sm:flex-row">
            <div className="flex flex-col gap-1 sm:w-32">
              {presets.map((p) => (
                <Button
                  key={p.label}
                  variant={p.active ? "secondary" : "ghost"}
                  size="sm"
                  className="justify-start font-normal"
                  onClick={() => applyPreset(p.from, p.to)}
                >
                  {p.label}
                </Button>
              ))}
            </div>
            <DayPicker
              mode="range"
              numberOfMonths={2}
              defaultMonth={defaultMonth}
              selected={selected}
              onSelect={onSelect}
              // Days outside the VOD Archive's bounds hold nothing to filter.
              disabled={[
                ...(min ? [{ before: min }] : []),
                ...(max ? [{ after: max }] : []),
              ]}
              showOutsideDays={false}
            />
          </div>
        </PopoverContent>
      </Popover>
      {active && (
        <Button
          variant="ghost"
          size="icon"
          className="ml-1 h-8 w-8 text-muted-foreground"
          onClick={() => onChange("", "")}
          title="Clear date filter"
          aria-label="Clear date filter"
        >
          <X className="h-4 w-4" />
        </Button>
      )}
    </div>
  )
}

import { useCallback, useLayoutEffect, useRef } from "react"

import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"

export const DEFAULT_OUTPUT_TEMPLATE = "{platform}/{username}/{datetime}"

// Recognized output-template placeholders. Highlighting only the known ones gives
// live feedback so typos ({usrname}) stay un-highlighted and visibly wrong.
const TEMPLATE_VAR =
  /\{(?:platform|username|session_id|datetime|date|time|utc_offset|timestamp|year|month|day|hour|minute|second)\}/
// Global+capturing variant for splitting; kept separate so .test() below isn't
// affected by lastIndex statefulness.
const TEMPLATE_VAR_SPLIT = new RegExp(`(${TEMPLATE_VAR.source})`, "g")

function renderHighlighted(text: string) {
  return text.split(TEMPLATE_VAR_SPLIT).map((part, i) =>
    TEMPLATE_VAR.test(part) ? (
      <span key={i} className="text-blue-400">
        {part}
      </span>
    ) : (
      <span key={i}>{part}</span>
    ),
  )
}

// OutputTemplateInput is a text input that renders recognized {variables} in blue
// via an overlay aligned to the input (matching padding, font, size, and a
// transparent 1px border for the input's border); the input text itself is made
// transparent so only the highlighted overlay shows.
export function OutputTemplateInput({
  value,
  onChange,
  placeholder = DEFAULT_OUTPUT_TEMPLATE,
  className,
}: {
  value: string
  onChange: (value: string) => void
  placeholder?: string
  className?: string
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  const overlayRef = useRef<HTMLDivElement>(null)

  // The native input scrolls horizontally to keep the caret visible, but the
  // overlay is a separate scroll context that would otherwise stay pinned at the
  // start. Mirror the input's scroll onto the overlay so long templates follow
  // the caret instead of disappearing off the right edge.
  const syncScroll = useCallback(() => {
    if (inputRef.current && overlayRef.current) {
      overlayRef.current.scrollLeft = inputRef.current.scrollLeft
    }
  }, [])

  // Typing grows the overlay and scrolls the input on the same render; realign
  // after the commit (onScroll alone can fire before the overlay content updates).
  useLayoutEffect(() => {
    syncScroll()
  }, [value, syncScroll])

  return (
    <div className={cn("relative min-w-0", className)}>
      <div
        ref={overlayRef}
        aria-hidden
        className="pointer-events-none absolute inset-0 overflow-hidden whitespace-pre rounded-md border border-transparent px-3 py-1 font-mono text-sm leading-6"
      >
        {renderHighlighted(value)}
      </div>
      <Input
        ref={inputRef}
        className="relative bg-transparent font-mono text-sm leading-6 text-transparent caret-foreground"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        onScroll={syncScroll}
      />
    </div>
  )
}

export default OutputTemplateInput

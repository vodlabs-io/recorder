import { useEffect, useState } from "react"
import { Loader2 } from "lucide-react"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { getDaemonClient } from "@/lib/daemon"
import { LazyThumb } from "./lazy-thumb"

// Contact sheets are ~200-700 KB each; keep the last few opened.
const SHEET_CACHE_MAX = 6
const sheets = new Map<string, Promise<string | null>>()

function loadSheet(dir: string): Promise<string | null> {
  let p = sheets.get(dir)
  if (!p) {
    p = getDaemonClient()
      .getContactSheet(dir)
      .then((b64) => `data:image/jpeg;base64,${b64}`)
      .catch(() => {
        sheets.delete(dir)
        return null
      })
  }
  sheets.delete(dir)
  sheets.set(dir, p)
  for (const k of sheets.keys()) {
    if (sheets.size <= SHEET_CACHE_MAX) break
    sheets.delete(k)
  }
  return p
}

// ContactSheet shows a finished VOD's contact sheet, one across all its
// files, in its info panel and opens it full size on click. The thumbnail
// stands in while the sheet loads, and for VODs recorded before contact
// sheets existed, which have none.
export function ContactSheet({ dir, thumbFile }: { dir: string; thumbFile: string | null }) {
  const [url, setUrl] = useState<string | null>(null)
  const [state, setState] = useState<"loading" | "ready" | "failed">("loading")
  const [open, setOpen] = useState(false)

  useEffect(() => {
    let alive = true
    setState("loading")
    setUrl(null)
    void loadSheet(dir).then((u) => {
      if (!alive) return
      setUrl(u)
      setState(u ? "ready" : "failed")
    })
    return () => {
      alive = false
    }
  }, [dir])

  return (
    <div className="relative shrink-0 overflow-hidden rounded-lg border bg-black">
      {url ? (
        <button
          type="button"
          className="block w-full cursor-zoom-in"
          onClick={() => setOpen(true)}
          title="Show full size"
        >
          <img src={url} alt="Contact sheet" className="h-auto w-full" draggable={false} />
        </button>
      ) : (
        <>
          <LazyThumb dir={dir} file={thumbFile} />
          {state === "loading" && (
            <div className="absolute right-2 bottom-2 flex items-center gap-1.5 rounded bg-black/60 px-2 py-1 text-xs text-white">
              <Loader2 className="h-3 w-3 animate-spin" />
              Contact sheet
            </div>
          )}
        </>
      )}
      <Dialog open={open} onOpenChange={setOpen}>
        {/* Fills the viewport so a click anywhere off the sheet closes it. */}
        <DialogContent
          showCloseButton={false}
          className="inset-0 top-0 left-0 flex h-dvh w-screen max-w-none translate-x-0 translate-y-0 items-center justify-center rounded-none bg-transparent p-4 ring-0 sm:max-w-none"
          onClick={(e) => {
            if (e.target === e.currentTarget) setOpen(false)
          }}
        >
          <DialogTitle className="sr-only">Contact sheet</DialogTitle>
          {url && (
            <img
              src={url}
              alt=""
              className="max-h-full max-w-full rounded-md object-contain"
              draggable={false}
            />
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}

import { useEffect, useState } from "react"
import { Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { getDaemonClient, type VODFile } from "@/lib/daemon"
import { LazyThumb } from "./lazy-thumb"

// Contact sheets are ~200-700 KB each; keep the last few opened.
const SHEET_CACHE_MAX = 6
const sheets = new Map<string, Promise<string | null>>()

function loadSheet(dir: string, file: string): Promise<string | null> {
  const key = `${dir}|${file}`
  let p = sheets.get(key)
  if (!p) {
    p = getDaemonClient()
      .getContactSheet(dir, file)
      .then((b64) => `data:image/jpeg;base64,${b64}`)
      .catch(() => {
        sheets.delete(key)
        return null
      })
  }
  sheets.delete(key)
  sheets.set(key, p)
  for (const k of sheets.keys()) {
    if (sheets.size <= SHEET_CACHE_MAX) break
    sheets.delete(k)
  }
  return p
}

// ContactSheet shows a finished VOD's contact sheet in its info panel, one
// file at a time, and opens it full size on click. Until the sheet arrives
// (the daemon generates a missing one on first view) the thumbnail stands in.
export function ContactSheet({ dir, files }: { dir: string; files: VODFile[] }) {
  const [index, setIndex] = useState(0)
  const [url, setUrl] = useState<string | null>(null)
  const [state, setState] = useState<"loading" | "ready" | "failed">("loading")
  const [open, setOpen] = useState(false)
  const file = files[Math.min(index, files.length - 1)]?.name

  useEffect(() => {
    if (!file) return
    let alive = true
    setState("loading")
    setUrl(null)
    void loadSheet(dir, file).then((u) => {
      if (!alive) return
      setUrl(u)
      setState(u ? "ready" : "failed")
    })
    return () => {
      alive = false
    }
  }, [dir, file])

  return (
    <div className="flex shrink-0 flex-col gap-2">
      {files.length > 1 && (
        <div className="flex flex-wrap gap-1">
          {files.map((f, i) => (
            <Button
              key={f.name}
              size="xs"
              variant={i === index ? "secondary" : "ghost"}
              onClick={() => setIndex(i)}
            >
              Part {i + 1}
            </Button>
          ))}
        </div>
      )}
      <div className="relative overflow-hidden rounded-lg border bg-black">
        {url ? (
          <button
            type="button"
            className="block w-full cursor-zoom-in"
            onClick={() => setOpen(true)}
            title="Show full size"
          >
            <img src={url} alt={`Contact sheet of ${file}`} className="h-auto w-full" draggable={false} />
          </button>
        ) : (
          <>
            <LazyThumb dir={dir} file={file ?? null} />
            {state === "loading" && (
              <div className="absolute right-2 bottom-2 flex items-center gap-1.5 rounded bg-black/60 px-2 py-1 text-xs text-white">
                <Loader2 className="h-3 w-3 animate-spin" />
                Contact sheet
              </div>
            )}
          </>
        )}
      </div>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-h-[95vh] overflow-auto p-2 sm:max-w-[min(95vw,1600px)]">
          <DialogTitle className="sr-only">Contact sheet of {file}</DialogTitle>
          {url && <img src={url} alt="" className="h-auto w-full rounded-md" draggable={false} />}
        </DialogContent>
      </Dialog>
    </div>
  )
}

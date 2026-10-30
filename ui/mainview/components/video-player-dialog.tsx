import { lazy, Suspense } from "react"
import { Download, Loader2 } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { getDaemonClient } from "@/lib/daemon"
import { downloadVOD, playerStore } from "@/lib/play-vod"

const VodPlayer = lazy(() => import("./vod-player"))

// VideoPlayerDialog plays a recording in the page, for hosts with no OS video
// player to hand it to (the Web UI). The daemon serves the file with Range
// support, so seeking works, and its timeline previews for the seek bar.
export function VideoPlayerDialog() {
  const { dir, file, open } = playerStore()
  const client = getDaemonClient()
  const src = open ? client.mediaUrl(dir, file) : ""
  const track = open ? client.previewTrackUrl(dir, file) : ""

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => playerStore.setState({ open: o })}
    >
      <DialogContent className="sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle className="truncate pr-8 font-mono text-sm">{file}</DialogTitle>
        </DialogHeader>
        {src && (
          <Suspense
            fallback={
              <div className="flex aspect-video w-full items-center justify-center rounded-md bg-black">
                <Loader2 className="h-6 w-6 animate-spin text-white/60" />
              </div>
            }
          >
            <VodPlayer src={src} track={track || undefined} />
          </Suspense>
        )}
        <div className="flex justify-end">
          <Button variant="outline" onClick={() => downloadVOD(dir, file)}>
            <Download className="h-4 w-4" />
            Download
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}

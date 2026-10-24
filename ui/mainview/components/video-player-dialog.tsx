import { Download } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { getDaemonClient } from "@/lib/daemon"
import { downloadVOD, playerStore } from "@/lib/play-vod"

// VideoPlayerDialog plays a recording in the page, for hosts with no OS video
// player to hand it to (the Web UI). The daemon serves the file with Range
// support, so seeking works.
export function VideoPlayerDialog() {
  const { dir, file, open } = playerStore()
  const src = open ? getDaemonClient().mediaUrl(dir, file) : ""

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
          <video
            key={src}
            src={src}
            controls
            autoPlay
            playsInline
            className="max-h-[70vh] w-full rounded-md bg-black"
          />
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

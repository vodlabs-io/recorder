import { lazy, Suspense, useEffect, useState } from "react"
import { Download, Loader2 } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { getDaemonClient, type RecordingStreamUrls } from "@/lib/daemon"
import { downloadVOD, playerStore } from "@/lib/play-vod"

const VodPlayer = lazy(() => import("./vod-player"))

// The player's source: the whole recording as one stream when the daemon
// can stream it, else the chosen file alone.
type Source = { stream: RecordingStreamUrls } | { file: true }

// VideoPlayerDialog plays a recording in the page, for hosts with no OS video
// player to hand it to (the Web UI): all its files in order on one timeline,
// starting at the one chosen, or that file alone when the recording holds a
// file that can't be streamed (WebM). The daemon serves files with Range
// support, so seeking works either way.
export function VideoPlayerDialog() {
  const { dir, file, open } = playerStore()
  const client = getDaemonClient()
  const [source, setSource] = useState<Source | null>(null)

  useEffect(() => {
    setSource(null)
    if (!open) return
    const stream = client.recordingStreamUrls(dir, file)
    if (!stream) {
      setSource({ file: true })
      return
    }
    let alive = true
    void fetch(stream.playlist, { cache: "no-store" })
      .then((res) => res.ok)
      .catch(() => false)
      .then((ok) => {
        if (alive) setSource(ok ? { stream } : { file: true })
      })
    return () => {
      alive = false
    }
  }, [client, dir, file, open])

  const whole = source && "stream" in source
  const title = whole ? dir.split(/[\\/]/).filter(Boolean).pop() || file : file

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => playerStore.setState({ open: o })}
    >
      <DialogContent className="sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle className="truncate pr-8 font-mono text-sm">{title}</DialogTitle>
        </DialogHeader>
        <Suspense fallback={<PlayerPlaceholder />}>
          {!source ? (
            <PlayerPlaceholder />
          ) : "stream" in source ? (
            <VodPlayer stream={source.stream} src="" />
          ) : (
            <VodPlayer
              src={client.mediaUrl(dir, file)}
              track={client.previewTrackUrl(dir, file) || undefined}
            />
          )}
        </Suspense>
        <div className="flex justify-end">
          <Button variant="outline" onClick={() => downloadVOD(dir, file)}>
            <Download className="h-4 w-4" />
            Download {file}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function PlayerPlaceholder() {
  return (
    <div className="flex aspect-video w-full items-center justify-center rounded-md bg-black">
      <Loader2 className="h-6 w-6 animate-spin text-white/60" />
    </div>
  )
}

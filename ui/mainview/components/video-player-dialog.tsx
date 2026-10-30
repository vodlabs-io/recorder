import { lazy, Suspense, useEffect, useState } from "react"
import { Download, ExternalLink, Loader2 } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { getDaemonClient, type RecordingStreamUrls } from "@/lib/daemon"
import { canDownloadVODs, downloadVOD, hasPlayerChoice, playerStore, playVOD } from "@/lib/play-vod"

const VodPlayer = lazy(() => import("./vod-player"))

// The player's source: the whole recording as one stream when the daemon
// can stream it, else the chosen file alone.
type Source = { stream: RecordingStreamUrls } | { file: true }

// VideoPlayerDialog plays a recording in the app: all its files in order on
// one timeline, starting at the one chosen, or that file alone when the
// recording holds a file that can't be streamed (WebM). The daemon serves
// files with Range support, so seeking works either way.
export function VideoPlayerDialog() {
  const { dir, file, title, subtitle, open } = playerStore()
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


  return (
    <Dialog
      open={open}
      onOpenChange={(o) => playerStore.setState({ open: o })}
    >
      <DialogContent className="sm:max-w-4xl">
        <DialogHeader className="min-w-0 gap-0.5 pr-8">
          <DialogTitle className="line-clamp-2 break-words text-base" title={title}>
            {title}
          </DialogTitle>
          {subtitle && <DialogDescription className="truncate">{subtitle}</DialogDescription>}
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
        <div className="flex justify-end gap-2">
          {hasPlayerChoice() && (
            <Button
              variant="outline"
              onClick={() => {
                playerStore.setState({ open: false })
                playVOD(dir, file, { player: "os" })
              }}
            >
              <ExternalLink className="h-4 w-4" />
              Open {file} in system player
            </Button>
          )}
          {canDownloadVODs() && (
            <Button variant="outline" onClick={() => downloadVOD(dir, file)}>
              <Download className="h-4 w-4" />
              Download {file}
            </Button>
          )}
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

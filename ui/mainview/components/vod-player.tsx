import "@videojs/react/video/skin.css"
import "./vod-player.css"
import { useCallback, useRef } from "react"
import { createPlayer } from "@videojs/react"
import { HlsJsVideo } from "@videojs/react/media/hlsjs-video"
import { Video, VideoSkin, videoFeatures } from "@videojs/react/video"
import type { HlsJsAdapter } from "@videojs/hlsjs-video"
import type { RecordingStreamUrls } from "@/lib/daemon"
import { useTrackBlob } from "@/lib/thumbnails-track"

const { Player } = createPlayer({ features: videoFeatures })

// VodPlayer plays a recording with the video.js skin: with stream, every
// file in order on one timeline (a chapter per file splits the seek bar);
// otherwise the one file at src. track, when set, is that file's
// timeline-preview track. Tracks load as blobs, since the media may come
// from another origin. Kept in its own module so the player loads only when
// a VOD plays.
export default function VodPlayer({
  stream,
  src,
  track,
}: {
  stream?: RecordingStreamUrls
  src: string
  track?: string
}) {
  const chapters = useTrackBlob(stream?.chapters)
  const thumbnails = useTrackBlob(stream ? stream.thumbnails : track)
  const recovery = useDecodeRecovery()
  return (
    <Player>
      <VideoSkin className="vod-player max-h-[70vh] w-full overflow-hidden rounded-md bg-black">
        {stream ? (
          <HlsJsVideo
            key={stream.playlist}
            ref={recovery.video}
            mediaRef={recovery.adapter}
            src={stream.playlist}
            autoPlay
            playsInline
          >
            {chapters && <track key={chapters} kind="chapters" src={chapters} default />}
            {thumbnails && <track key={thumbnails} kind="metadata" label="thumbnails" src={thumbnails} default />}
          </HlsJsVideo>
        ) : (
          <Video key={src} src={src} autoPlay playsInline>
            {thumbnails && <track key={thumbnails} kind="metadata" label="thumbnails" src={thumbnails} default />}
          </Video>
        )}
      </VideoSkin>
    </Player>
  )
}

// useDecodeRecovery returns refs for an HlsJsVideo that rebuild hls.js's
// MediaSource when the browser fails to decode the stream, resuming where it
// stopped. WebKit fails a seek this way when it lands in a file encoded at
// another H.264 level than the one just played; hls.js then also reports its
// next append, against the closed MediaSource, as a fatal error. Both reach
// the player through the adapter's dispatchEvent, so the hook holds back
// decode errors there until the rebuilt MediaSource is attached. A failure
// before playback gets past the position it resumed from goes on to the
// player's error dialog.
function useDecodeRecovery() {
  const el = useRef<HTMLVideoElement | null>(null)
  const video = useCallback((node: HTMLVideoElement | null) => {
    el.current = node
  }, [])
  const adapter = useCallback((media: HlsJsAdapter | null) => {
    if (!media) return
    const dispatch = media.dispatchEvent.bind(media)
    let resumedAt: number | null = null
    let reattaching = false
    const isDecodeError = (event: Event) =>
      (event as ErrorEvent).error?.code === MediaError.MEDIA_ERR_DECODE ||
      el.current?.error?.code === MediaError.MEDIA_ERR_DECODE
    media.dispatchEvent = (event: Event) => {
      const v = el.current
      if (event.type === "timeupdate" && v && resumedAt !== null && Math.abs(v.currentTime - resumedAt) > 1) {
        resumedAt = null
      }
      if (event.type !== "error" || !isDecodeError(event)) return dispatch(event)
      if (reattaching) return true
      const engine = media.engine
      if (!engine || !v || resumedAt !== null) return dispatch(event)
      resumedAt = v.currentTime
      reattaching = true
      const resume = !v.paused
      engine.once("hlsMediaAttached" as Parameters<typeof engine.once>[0], () => {
        reattaching = false
        if (resume) void v.play().catch(() => {})
      })
      engine.recoverMediaError()
      return true
    }
    return () => {
      media.dispatchEvent = dispatch
    }
  }, [])
  return { video, adapter }
}

import "@videojs/react/video/skin.css"
import { createPlayer } from "@videojs/react"
import { HlsJsVideo } from "@videojs/react/media/hlsjs-video"
import { Video, VideoSkin, videoFeatures } from "@videojs/react/video"
import type { RecordingStreamUrls } from "@/lib/daemon"
import { useThumbnailsTrack } from "@/lib/thumbnails-track"

const { Player } = createPlayer({ features: videoFeatures })

// VodPlayer plays a recording with the video.js skin: with stream, every
// file in order on one timeline (a chapter per file splits the seek bar);
// otherwise the one file at src. track, when set, is that file's
// timeline-preview track. Kept in its own module so the player loads only
// when a VOD plays.
export default function VodPlayer({
  stream,
  src,
  track,
}: {
  stream?: RecordingStreamUrls
  src: string
  track?: string
}) {
  const thumbnails = useThumbnailsTrack(stream?.thumbnails)
  return (
    <Player>
      <VideoSkin className="max-h-[70vh] w-full overflow-hidden rounded-md bg-black">
        {stream ? (
          <HlsJsVideo key={stream.playlist} src={stream.playlist} autoPlay playsInline>
            <track kind="chapters" src={stream.chapters} default />
            {thumbnails && <track key={thumbnails} kind="metadata" label="thumbnails" src={thumbnails} default />}
          </HlsJsVideo>
        ) : (
          <Video key={src} src={src} autoPlay playsInline>
            {track && <track kind="metadata" label="thumbnails" src={track} default />}
          </Video>
        )}
      </VideoSkin>
    </Player>
  )
}

import "@videojs/react/video/skin.css"
import { createPlayer } from "@videojs/react"
import { Video, VideoSkin, videoFeatures } from "@videojs/react/video"

const { Player } = createPlayer({ features: videoFeatures })

// VodPlayer plays one VOD file with the video.js skin. track, when set, is
// the file's timeline-preview track: the seek bar shows its sprite under the
// pointer. Kept in its own module so the player loads only when a VOD plays.
export default function VodPlayer({ src, track }: { src: string; track?: string }) {
  return (
    <Player>
      <VideoSkin className="max-h-[70vh] w-full overflow-hidden rounded-md bg-black">
        <Video key={src} src={src} autoPlay playsInline>
          {track && <track kind="metadata" label="thumbnails" src={track} default />}
        </Video>
      </VideoSkin>
    </Player>
  )
}

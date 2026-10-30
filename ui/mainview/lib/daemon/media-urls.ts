import type { RecordingStreamUrls } from "./client"

// MediaUrls are the daemon's recording routes: a file, its timeline-preview
// track, and a whole recording as one stream.
export type MediaUrls = {
  media: (dir: string, file: string, download?: boolean) => string
  previewTrack: (dir: string, file: string) => string
  stream: (dir: string, start: string) => RecordingStreamUrls
}

// mediaUrls resolves the recording routes against base: the Web UI's page,
// or the desktop player's loopback listener.
export function mediaUrls(base: string): MediaUrls {
  const at = (path: string) => new URL(path, base)
  return {
    media: (dir, file, download = false) => {
      const u = at("api/vods/file")
      u.searchParams.set("dir", dir)
      u.searchParams.set("file", file)
      if (download) u.searchParams.set("download", "1")
      return u.href
    },
    previewTrack: (dir, file) => {
      const u = at("api/vods/preview")
      u.searchParams.set("dir", dir)
      u.searchParams.set("file", file)
      u.searchParams.set("kind", "vtt")
      return u.href
    },
    stream: (dir, start) => {
      const route = (name: string) => {
        const u = at("api/vods/hls/" + name)
        u.searchParams.set("dir", dir)
        return u
      }
      const playlist = route("index.m3u8")
      playlist.searchParams.set("start", start)
      return {
        playlist: playlist.href,
        chapters: route("chapters.vtt").href,
        thumbnails: route("thumbs.vtt").href,
      }
    },
  }
}

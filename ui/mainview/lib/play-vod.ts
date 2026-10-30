import { create } from "zustand"
import { getDaemonClient } from "@/lib/daemon"
import { joinPath } from "@/lib/utils"
import { appStore } from "@/state/app-state"

// Playing a recording means the app's own player, or on the desktop the OS
// video player when the user prefers it; this module picks between them.

// The file the in-page player shows, and the title over it.
export const playerStore = create<{
  dir: string
  file: string
  title: string
  subtitle?: string
  open: boolean
}>(() => ({ dir: "", file: "", title: "", open: false }))

// Player is where a recording plays: the app's own player, or the OS one.
export type Player = "app" | "os"

// PlayOptions pick the player (by default the user's) and what the app's
// player shows over the video: the stream title, and a line under it. A
// missing title falls back to the file name.
export type PlayOptions = { player?: Player; title?: string; subtitle?: string }

// canPlayVODs reports whether this host can play a recording at all.
export function canPlayVODs(): boolean {
  const c = getDaemonClient().capabilities
  return c.openPath || c.inPagePlayer
}

// hasPlayerChoice reports whether this host has both players (the desktop
// app), so the user can pick a default and reach the other.
export function hasPlayerChoice(): boolean {
  const c = getDaemonClient().capabilities
  return c.openPath && c.inPagePlayer
}

// defaultPlayer is where a recording plays unless asked otherwise: the
// user's choice where there is one, else whichever player the host has.
export function defaultPlayer(): Player {
  if (hasPlayerChoice()) return appStore.getState().prefs.player ?? "app"
  return getDaemonClient().capabilities.inPagePlayer ? "app" : "os"
}

// useDefaultPlayer is defaultPlayer, re-rendering when the user changes it.
export function useDefaultPlayer(): Player {
  const pref = appStore((s) => s.prefs.player)
  if (hasPlayerChoice()) return pref ?? "app"
  return getDaemonClient().capabilities.inPagePlayer ? "app" : "os"
}

// canDownloadVODs reports whether recordings can be saved from this host,
// which only means something when they live on another machine.
export function canDownloadVODs(): boolean {
  return getDaemonClient().capabilities.browserMedia
}

// playVOD plays one file of a recording. The app's player plays the whole
// recording from that file on; if it can't be reached, the OS player takes
// over where there is one.
export function playVOD(dir: string, file: string, opts: PlayOptions = {}): void {
  const player = opts.player ?? defaultPlayer()
  const client = getDaemonClient()
  if (player === "os" && client.capabilities.openPath) {
    void client.openPath(joinPath(dir, file))
    return
  }
  if (!client.capabilities.inPagePlayer) return
  void client.prepareMedia().then((ok) => {
    if (ok) playerStore.setState({ dir, file, title: opts.title || file, subtitle: opts.subtitle, open: true })
    else if (client.capabilities.openPath) void client.openPath(joinPath(dir, file))
  })
}

export function downloadVOD(dir: string, file: string): void {
  const url = getDaemonClient().mediaUrl(dir, file, true)
  if (!url) return
  const a = document.createElement("a")
  a.href = url
  a.download = file
  a.rel = "noopener"
  document.body.appendChild(a)
  a.click()
  a.remove()
}

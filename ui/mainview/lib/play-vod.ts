import { create } from "zustand"
import { getDaemonClient } from "@/lib/daemon"
import { joinPath } from "@/lib/utils"

// Playing a recording means the OS video player on the desktop and an
// in-page <video> in the Web UI; this module picks the one the host has.

// The file the in-page player shows, or null when it is closed.
export const playerStore = create<{
  dir: string
  file: string
  open: boolean
}>(() => ({ dir: "", file: "", open: false }))

// canPlayVODs reports whether this host can play a recording at all.
export function canPlayVODs(): boolean {
  const c = getDaemonClient().capabilities
  return c.openPath || c.browserMedia
}

// canDownloadVODs reports whether recordings can be saved from this host,
// which only means something when they live on another machine.
export function canDownloadVODs(): boolean {
  return getDaemonClient().capabilities.browserMedia
}

export function playVOD(dir: string, file: string): void {
  const client = getDaemonClient()
  if (client.capabilities.openPath) {
    void client.openPath(joinPath(dir, file))
    return
  }
  if (client.capabilities.browserMedia) {
    playerStore.setState({ dir, file, open: true })
  }
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

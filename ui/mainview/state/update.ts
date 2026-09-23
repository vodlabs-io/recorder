import { getDaemonClient, type UpdateState } from "@/lib/daemon"

// UpdateSlice holds the state behind the Update tab and the sidebar badge.
//
// On the desktop the whole thing is owned by the bun process — it checks both
// halves on a background loop, auto-downloads what it finds, and runs the
// install sequence — and arrives here as one pushed UpdateState. Hosts that
// can't install (the Web UI) synthesize a read-only state from the
// snapshot instead, so the tab still reports where the daemon stands.
export interface UpdateSlice {
  updateState: UpdateState
  updatesInit: boolean
  initUpdates: () => void
  // Re-check both halves. Called when the Update tab mounts so opening it never
  // shows a stale answer; the background loop covers everything else.
  checkUpdates: () => Promise<void>
  // Install whatever is downloaded: daemon first (restarted in place), then the
  // app (installed, and the app relaunches).
  installUpdates: () => Promise<void>
}

const EMPTY_UPDATE_STATE: UpdateState = {
  app: {
    phase: "idle",
    currentVersion: "",
    channel: "",
    availableVersion: null,
    progress: null,
    message: null,
  },
  daemon: {
    phase: "idle",
    currentVersion: "",
    availableVersion: null,
    progress: null,
    message: null,
    container: false,
  },
  install: { phase: "idle", message: null },
}

// selectUpdateAvailable is the one definition of "something is waiting" — used
// by the sidebar badge and the Update tab, so they can't drift apart.
// availableVersion is set from the moment a newer version is found and cleared
// once it's installed, which covers downloading and ready alike, and works for
// read-only hosts that never download at all.
export function selectUpdateAvailable(s: { updateState: UpdateState }): boolean {
  return (
    s.updateState.app.availableVersion !== null ||
    s.updateState.daemon.availableVersion !== null
  )
}

export const createUpdateSlice = (set: any, get: any): UpdateSlice => ({
  updateState: EMPTY_UPDATE_STATE,
  updatesInit: false,

  initUpdates: () => {
    if (get().updatesInit) return
    set({ updatesInit: true })
    const client = getDaemonClient()
    client.onUpdateStateChanged((updateState) => set({ updateState }))
    client
      .getUpdateState()
      .then((updateState) => set({ updateState }))
      .catch(() => {
        // bun not ready yet; the push will deliver the first state.
      })
  },

  checkUpdates: async () => {
    try {
      set({ updateState: await getDaemonClient().checkUpdates() })
    } catch {
      // A failed check leaves the last known state in place; the background
      // loop will try again.
    }
  },

  installUpdates: async () => {
    // The bun process reports progress and any failure through the pushed
    // install phase, so there's nothing to do with the return value here.
    try {
      await getDaemonClient().installUpdates()
    } catch (e) {
      console.error("installUpdates failed:", e)
    }
  },
})

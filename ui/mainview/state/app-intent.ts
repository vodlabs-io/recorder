import { getDaemonClient } from "@/lib/daemon"

// AppIntentSlice carries out what the host asks the UI to show. Screens are a
// hash change away; the Add Streamer dialog lives on a screen that may not be
// mounted yet, so its request waits here for the dialog to pick it up.
export interface AppIntentSlice {
  addStreamerRequested: boolean
  installConfirmRequested: boolean
  appIntentsInit: boolean
  initAppIntents: () => void
  clearAddStreamerRequest: () => void
  clearInstallConfirmRequest: () => void
}

export const createAppIntentSlice = (set: any, get: any): AppIntentSlice => ({
  addStreamerRequested: false,
  installConfirmRequested: false,
  appIntentsInit: false,

  initAppIntents: () => {
    if (get().appIntentsInit) return
    set({ appIntentsInit: true })
    const client = getDaemonClient()
    const take = async () => {
      const intent = await client.takeAppIntent().catch(() => null)
      if (!intent) return
      window.location.hash = intent.route
      // A plain screen change leaves the dialog's screen, so it drops the request
      // too; otherwise the dialog would reopen on coming back.
      set({
        addStreamerRequested: !!intent.addStreamer,
        installConfirmRequested: !!intent.confirmInstall,
      })
    }
    client.onAppIntentPending(() => void take())
    // A window opened for an intent mounts after the push that announced it.
    void take()
  },

  clearAddStreamerRequest: () => set({ addStreamerRequested: false }),
  clearInstallConfirmRequest: () => set({ installConfirmRequested: false }),
})

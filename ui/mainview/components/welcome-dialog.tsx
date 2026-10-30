import { useEffect, useState } from "react"
import { ExternalLink, FolderOpen, Users } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import { appStore } from "@/state/app-state"
import { getDaemonClient } from "@/lib/daemon"
import { refreshErrorReports, setErrorReports, useErrorReports } from "@/lib/error-reports"

// WelcomeDialog is the app's one chance to say what it does before a user
// starts using it: where recordings land, that it has to keep running to catch
// a stream, and what the free service is given in return. Each of those is
// discoverable afterwards, but a user who never goes looking should not have
// to find out from behaviour.
//
// Shown once per client (prefs.welcome_seen), and only after the daemon is
// connected — every step needs an answer from it.

type Step = 0 | 1 | 2

export function WelcomeDialog() {
  const prefsHydrated = appStore((s) => s.prefsHydrated)
  const seen = appStore((s) => s.prefs.welcome_seen)
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"

  const [step, setStep] = useState<Step>(0)
  const [downloadDir, setDownloadDir] = useState<string>("")
  const [startAtLogin, setStartAtLogin] = useState<boolean | null>(null)
  const [loginSupported, setLoginSupported] = useState(false)
  const reports = useErrorReports()
  // Off unless the user turns it on here; recorded when the dialog closes.
  const [sendReports, setSendReports] = useState(false)

  const open = prefsHydrated && !seen && connected

  useEffect(() => {
    if (!open) return
    getDaemonClient()
      .getInfo()
      .then((i) => setDownloadDir(i.download_dir))
      .catch(() => setDownloadDir(""))
    getDaemonClient()
      .getStartAtLogin()
      .then((s) => {
        setLoginSupported(s.supported)
        setStartAtLogin(s.enabled)
      })
      .catch(() => setLoginSupported(false))
    void refreshErrorReports()
  }, [open])

  const finish = () => {
    appStore.getState().persistPrefs({ welcome_seen: true })
    // Closing the dialog is the answer, so the banner doesn't ask again.
    if (reports?.available && reports.enabled === null) {
      void setErrorReports(sendReports).catch(() => {})
    }
  }

  const chooseFolder = async () => {
    const picked = await getDaemonClient().chooseDirectory(downloadDir || undefined)
    if (!picked) return
    const cfg = await getDaemonClient().getConfig()
    await getDaemonClient().setConfig({ ...cfg, download_dir: picked })
    setDownloadDir(picked)
  }

  const toggleStartAtLogin = async (enabled: boolean) => {
    const s = await getDaemonClient().setStartAtLogin(enabled)
    setStartAtLogin(s.enabled)
  }

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) finish() }}>
      <DialogContent className="sm:max-w-lg">
        {step === 0 && (
          <>
            <DialogHeader>
              <DialogTitle>Welcome to VodLabs Recorder</DialogTitle>
              <DialogDescription>
                Add the streamers you follow, and their broadcasts are automatically recorded
                as they go live.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-3 text-sm text-muted-foreground">
              <p>
                Recording happens locally on this machine. The
                VodLabs Cloud service monitors streamers and notifies you when 
                they're live.
              </p>
              <p>
                If you sign in to a platform on the Accounts page, that login is stored on this
                device, encrypted, and is never sent to VodLabs.
              </p>
              <p>
                <Button
                  variant="link"
                  className="h-auto p-0 text-sm"
                  onClick={() =>
                    getDaemonClient().openExternal("https://vodlabs.io/privacy")
                  }
                >
                  Privacy Policy
                  <ExternalLink className="ml-1 h-3 w-3" />
                </Button>
              </p>
              {reports?.available && reports.enabled === null && (
                <div className="flex items-start justify-between gap-4 rounded-md border p-3">
                  <div className="space-y-1">
                    <p className="font-medium text-foreground">Send error reports</p>
                    <p>
                      When the recorder hits an error or crashes, it tells VodLabs what went
                      wrong so it can be fixed. Reports can name streamers and folders, never
                      your logins. You can change this later in Settings.
                    </p>
                  </div>
                  <Switch checked={sendReports} onCheckedChange={setSendReports} />
                </div>
              )}
            </div>
          </>
        )}

        {step === 1 && (
          <>
            <DialogHeader>
              <DialogTitle>Where recordings go</DialogTitle>
              <DialogDescription>
                Recordings are written here as they are captured. A local disk
                with room to spare works best.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-3">
              <p className="wrap-break-word rounded-md border bg-muted/40 p-3 font-mono text-xs">
                {downloadDir || "…"}
              </p>
              {getDaemonClient().capabilities.chooseDirectory && (
                <Button variant="outline" size="sm" onClick={chooseFolder}>
                  <FolderOpen className="mr-1 h-4 w-4" />
                  Choose a different folder
                </Button>
              )}
              <p className="text-sm text-muted-foreground">
                You can change this later in Settings.
              </p>
            </div>
          </>
        )}

        {step === 2 && (
          <>
            <DialogHeader>
              <DialogTitle>Catching streams</DialogTitle>
              <DialogDescription>
                Turn this on to auto-resume recordings after a reboot.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              {loginSupported && (
                <div className="flex items-start justify-between gap-4 rounded-md border p-3">
                  <div className="space-y-1 text-sm">
                    <p className="font-medium">Start at login</p>
                    <p className="text-muted-foreground">
                      Launches VodLabs Recorder minimized to the system tray when you log in to your computer.
                    </p>
                  </div>
                  <Switch
                    checked={startAtLogin ?? false}
                    disabled={startAtLogin === null}
                    onCheckedChange={toggleStartAtLogin}
                  />
                </div>
              )}
            </div>
          </>
        )}

        <DialogFooter className="sm:justify-between">
          <Button variant="ghost" size="sm" onClick={finish}>
            Skip
          </Button>
          <div className="flex gap-2">
            {step > 0 && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => setStep((step - 1) as Step)}
              >
                Back
              </Button>
            )}
            {step < 2 ? (
              <Button size="sm" onClick={() => setStep((step + 1) as Step)}>
                Next
              </Button>
            ) : (
              <Button size="sm" onClick={finish}>
                Add your first streamer
              </Button>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export default WelcomeDialog

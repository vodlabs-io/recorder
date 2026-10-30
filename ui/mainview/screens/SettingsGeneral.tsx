import { useEffect, useState } from "react";
import { Card, CardContent } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch"
import { SettingsRow, SettingsHint } from "@/components/settings-row"
import { useTheme, Theme } from "@/components/theme-provider"
import { getDaemonClient } from "@/lib/daemon"
import { refreshErrorReports, setErrorReports, useErrorReports } from "@/lib/error-reports"
import { PrivacyLink } from "@/components/error-reports-banner"
import { appStore } from "../state/app-state"

function SettingsGeneral() {
  const { theme, setTheme } = useTheme()
  // Read from the OS, not from a setting of ours: the user can turn this off
  // in their system settings, and the switch has to show what will happen
  // rather than what was last asked for. null until the first read answers.
  const [startAtLogin, setStartAtLogin] = useState<boolean | null>(null)
  const [supported, setSupported] = useState(false)
  const [busy, setBusy] = useState(false)
  // The daemon's answer to sending error reports; null until it has been read.
  const reports = useErrorReports()

  useEffect(() => {
    getDaemonClient()
      .getStartAtLogin()
      .then((s) => {
        setSupported(s.supported)
        setStartAtLogin(s.enabled)
      })
      .catch(() => setSupported(false))
    void refreshErrorReports()
  }, [])

  function handleChangeTheme(theme: Theme) {
    setTheme(theme)
    appStore.getState().persistPrefs({ theme })
  }

  async function handleStartAtLogin(enabled: boolean) {
    setBusy(true)
    try {
      const s = await getDaemonClient().setStartAtLogin(enabled)
      setSupported(s.supported)
      setStartAtLogin(s.enabled)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mx-auto my-4 flex w-full max-w-table px-4">
      <Card className="w-full">
        <CardContent className="flex">
          <div className="space-y-6">
            <SettingsRow
              label="Theme"
              control={
                <Select<Theme>
                  value={theme}
                  onValueChange={(value) => value && handleChangeTheme(value)}
                >
                  <SelectTrigger>
                    <SelectValue className="capitalize" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="light">Light</SelectItem>
                    <SelectItem value="dark">Dark</SelectItem>
                    <SelectItem value="system">System</SelectItem>
                  </SelectContent>
                </Select>
              }
            >
              <SettingsHint>System will follow your OS preferences.</SettingsHint>
            </SettingsRow>

            {supported && (
              <SettingsRow
                label="Start at login"
                control={
                  <Switch
                    checked={startAtLogin ?? false}
                    disabled={busy || startAtLogin === null}
                    onCheckedChange={handleStartAtLogin}
                  />
                }
              >
                <SettingsHint>
                  Launches VodLabs Recorder minimized to the system tray when you log in to your computer.
                </SettingsHint>
              </SettingsRow>
            )}

            {reports?.available && (
              <SettingsRow
                label="Send error reports"
                control={
                  <Switch
                    checked={reports.enabled === true}
                    onCheckedChange={(on) => void setErrorReports(on)}
                  />
                }
              >
                <SettingsHint>
                  Tells VodLabs when the recorder hits an error or crashes, so it can be
                  fixed. Reports can name streamers and folders, never your logins.{" "}
                  <PrivacyLink />
                </SettingsHint>
              </SettingsRow>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

export default SettingsGeneral

import { useEffect, useState } from "react"
import { appStore } from "@/state/app-state"
import { getDaemonClient, type UserConfig } from "@/lib/daemon"

// Load/patch/save state for the daemon's user config, shared by every Settings
// screen that edits it (Files, Recording, Processing). Each screen renders its
// own subset of fields but saves the whole config object, so the fields it
// doesn't show round-trip untouched.
export function useUserConfig() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"

  const [cfg, setCfg] = useState<UserConfig | null>(null)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // A containerized daemon's download folder is fixed by the Docker volume mount;
  // a host folder picked here wouldn't exist inside the container, so the picker
  // is disabled and the path is shown read-only.
  const [container, setContainer] = useState(false)
  // Hosts without a native folder picker (the Web UI) get free-text
  // path inputs instead — the daemon may not even run on the same machine.
  const canChoose = getDaemonClient().capabilities.chooseDirectory && !container

  useEffect(() => {
    if (!connected) return
    getDaemonClient()
      .getConfig()
      .then((c) => {
        setCfg(c)
        setError(null)
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
    getDaemonClient()
      .getInfo()
      .then((i) => setContainer(i.container))
      .catch(() => {})
  }, [connected])

  function update(patch: Partial<UserConfig>) {
    setCfg((c) => (c ? { ...c, ...patch } : c))
    setSaved(false)
  }

  async function save() {
    if (!cfg) return
    setSaving(true)
    setError(null)
    try {
      const applied = await getDaemonClient().setConfig(cfg)
      setCfg(applied)
      setSaved(true)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return { connected, cfg, update, save, saving, saved, error, container, canChoose }
}

export type UserConfigForm = ReturnType<typeof useUserConfig>

export default useUserConfig

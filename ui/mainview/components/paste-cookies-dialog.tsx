import { useEffect, useState } from "react"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { getDaemonClient } from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"

// PasteCookiesDialog is the "advanced" login path: the user pastes a raw Cookie
// header copied from their browser's dev tools. It's the only login method
// available in the Web UI (which can't read another site's cookies) and
// a fallback on the desktop.
//
// PRIVACY: the pasted cookies go only to the LOCAL daemon; they are never sent to
// the vodlabs cloud.
export function PasteCookiesDialog({
  platform,
  open,
  onOpenChange,
}: {
  platform: string | null
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const [value, setValue] = useState("")
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)

  // Reset the field whenever the dialog opens for a (possibly different) platform.
  useEffect(() => {
    if (open) {
      setValue("")
      setError(null)
      setSaving(false)
    }
  }, [open, platform])

  const save = async () => {
    if (!platform) return
    const cookieHeader = value.trim()
    if (!cookieHeader) {
      setError("Paste your Cookie header first.")
      return
    }
    setSaving(true)
    setError(null)
    try {
      await getDaemonClient().setPlatformCookies({ platform, cookieHeader })
      onOpenChange(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  const name = platform ? platformDisplayName(platform) : ""

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add {name} cookies</DialogTitle>
          <DialogDescription>
            Paste the <code>Cookie</code> header from a signed-in {name} tab in
            your browser (DevTools → Network → a request → Request Headers →
            Cookie). It stays on this device and is never uploaded.
          </DialogDescription>
        </DialogHeader>

        <textarea
          value={value}
          onChange={(e) => setValue(e.target.value)}
          rows={5}
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          placeholder="name=value; name2=value2; …"
          className="w-full resize-none rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm outline-none focus-visible:ring-1 focus-visible:ring-ring"
        />

        {error && <p className="text-xs text-destructive">{error}</p>}

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={saving}
          >
            Cancel
          </Button>
          <Button onClick={save} disabled={saving}>
            {saving ? "Saving…" : "Save login"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

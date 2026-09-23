import type { ReactNode } from "react"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Check } from "lucide-react"
import { ConnectionBanner } from "@/components/connection-banner"
import type { UserConfigForm } from "@/hooks/use-user-config"
import type { UserConfig } from "@/lib/daemon"

// Shared chrome for the Settings screens that edit the daemon's user config:
// connection banner, card, loading/disconnected placeholder, and the Save row.
// Children are a render prop so they only run once the config has loaded, which
// also narrows `cfg` to non-null for the fields inside.
export function SettingsConfigForm({
  form,
  header,
  children,
}: {
  form: UserConfigForm
  // Rendered above the settings card, for content that is not an editable
  // setting. It supplies its own card so it can render nothing when it has
  // nothing to say.
  header?: ReactNode
  children: (cfg: UserConfig) => ReactNode
}) {
  const { connected, cfg, save, saving, saved, error } = form

  return (
    <>
      <ConnectionBanner />
      <div className="mx-auto my-4 flex w-full max-w-table flex-col gap-4 px-4">
        {header}
        <Card className="w-full">
          <CardContent className="flex">
            {!cfg ? (
              <p className="text-sm text-muted-foreground">
                {connected ? "Loading settings…" : "Daemon not connected."}
              </p>
            ) : (
              <div className="space-y-6">
                {children(cfg)}

                {/* Save */}
                <div className="flex items-center gap-3 pt-2">
                  <Button onClick={save} disabled={saving}>
                    {saving ? "Saving…" : "Save"}
                  </Button>
                  {saved && (
                    <span className="flex items-center gap-1 text-sm text-muted-foreground">
                      <Check className="h-4 w-4" /> Saved — applies to new
                      recordings
                    </span>
                  )}
                  {error && <span className="text-sm text-destructive">{error}</span>}
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  )
}

export default SettingsConfigForm

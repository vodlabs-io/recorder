import { useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { RefreshCw } from "lucide-react"
import { SettingsRow, SettingsHint } from "@/components/settings-row"
import { SettingsConfigForm } from "@/components/settings-config-form"
import { useUserConfig } from "@/hooks/use-user-config"
import { cn } from "@/lib/utils"
import { getDaemonClient, type EncoderStatus } from "@/lib/daemon"

// The config stores the software fallback as "software"; the detected-encoder
// list carries it as its ffmpeg id "libx264". Normalize to the Select's value.
function encoderValue(stored: string): string {
  if (stored === "libx264") return "software"
  return stored || "auto"
}

// Friendly label for the trigger, resolved from the detected list when possible.
function encoderLabel(value: string | null, encoders: EncoderStatus[] | null): string {
  if (!value || value === "auto") return "Automatic (recommended)"
  if (value === "software") return "Software (libx264)"
  const match = encoders?.find((e) => e.id === value)
  return match?.name ?? value
}

// What happens to a recording after capture ends: merge/re-encode settings and
// cleanup of the raw parts.
export function SettingsProcessing() {
  const form = useUserConfig()
  const { connected, update } = form

  // Encoder candidates for the merge/re-encode dropdown, detected on the
  // daemon's machine (self-tested). null until first load.
  const [encoders, setEncoders] = useState<EncoderStatus[] | null>(null)
  const [encodersRetesting, setEncodersRetesting] = useState(false)

  useEffect(() => {
    if (!connected) return
    getDaemonClient()
      .listEncoders()
      .then((l) => setEncoders(l.encoders))
      .catch(() => setEncoders([]))
  }, [connected])

  async function retestEncoders() {
    setEncodersRetesting(true)
    try {
      const l = await getDaemonClient().listEncoders(true)
      setEncoders(l.encoders)
    } catch {
      /* leave the previous list in place */
    } finally {
      setEncodersRetesting(false)
    }
  }

  return (
    <SettingsConfigForm form={form}>
      {(cfg) => (
        <>
          <SettingsRow
            label="Video encoder"
            control={
              <div className="flex items-center gap-2">
                <Select
                  value={encoderValue(cfg.encoder)}
                  onValueChange={(v) => update({ encoder: v ?? "auto" })}
                >
                  <SelectTrigger className="h-9 w-64">
                    <SelectValue placeholder="Encoder">
                      {(value: string | null) => encoderLabel(value, encoders)}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="auto">Automatic (recommended)</SelectItem>
                    {(encoders ?? []).map((e) => {
                      const val = e.id === "libx264" ? "software" : e.id
                      const usable = !e.hardware || e.working
                      return (
                        <SelectItem key={e.id} value={val} disabled={!usable}>
                          {e.name}
                          {e.hardware &&
                            (e.working
                              ? ""
                              : e.available
                                ? " — not working"
                                : " — unavailable")}
                        </SelectItem>
                      )
                    })}
                  </SelectContent>
                </Select>
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="h-9"
                        disabled={!connected || encodersRetesting}
                        onClick={() => void retestEncoders()}
                      >
                        <RefreshCw
                          className={cn(
                            "h-4 w-4",
                            encodersRetesting && "animate-spin",
                          )}
                        />
                      </Button>
                    }
                  />
                  <TooltipContent>Re-test encoders</TooltipContent>
                </Tooltip>
              </div>
            }
          >
            <SettingsHint>
              Used when merging VOD files recorded at different resolutions.
              "Automatic" picks the fastest working hardware encoder on this
              machine, falling back to software. Hardware options that fail a
              self-test are disabled.
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Delete part files"
            control={
              <Checkbox
                checked={cfg.delete_part_files}
                onCheckedChange={(v) => update({ delete_part_files: v === true })}
              />
            }
          >
            <SettingsHint>
              Delete a Recording's raw parts once its VOD finishes processing.
            </SettingsHint>
          </SettingsRow>
        </>
      )}
    </SettingsConfigForm>
  )
}

export default SettingsProcessing

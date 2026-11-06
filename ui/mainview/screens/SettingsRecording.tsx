import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { SettingsRow, SettingsHint } from "@/components/settings-row"
import { SettingsConfigForm } from "@/components/settings-config-form"
import { useUserConfig } from "@/hooks/use-user-config"

// RESUME_CHOICES are the offered resume windows. A value set by hand in
// config.toml is shown as its own entry.
const RESUME_CHOICES: { value: string; label: string }[] = [
  { value: "off", label: "Off" },
  { value: "2m", label: "2 minutes" },
  { value: "5m", label: "5 minutes" },
  { value: "15m", label: "15 minutes (default)" },
  { value: "30m", label: "30 minutes" },
  { value: "1h", label: "1 hour" },
]

function resumeChoices(current: string): { value: string; label: string }[] {
  if (!current || RESUME_CHOICES.some((c) => c.value === current)) {
    return RESUME_CHOICES
  }
  return [...RESUME_CHOICES, { value: current, label: current }]
}

// How streams are captured: the default quality, the limits that split a
// long recording into multiple VOD files, and how long an interrupted
// recording waits to resume.
export function SettingsRecording() {
  const form = useUserConfig()
  const { update } = form

  return (
    <SettingsConfigForm form={form}>
      {(cfg) => (
        <>
          <SettingsRow
            label="Default variant"
            control={
              <Input
                value={cfg.variant}
                onChange={(e) => update({ variant: e.target.value })}
              />
            }
          >
            <SettingsHint>
              Quality to record by default (e.g. best, worst, 720p). Overridable
              per streamer.
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Max file size"
            control={
              <Input
                placeholder='e.g. "2GB", "500MB"'
                value={cfg.split_size}
                onChange={(e) => update({ split_size: e.target.value })}
              />
            }
          >
            <SettingsHint>
              Split recordings into multiple VOD files once this raw size is
              reached (e.g. "2GB"). Leave empty for no size limit. Override per
              streamer in Advanced options ("off" disables).
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Max duration"
            control={
              <Input
                placeholder='e.g. "1h30m", "45m"'
                value={cfg.split_duration}
                onChange={(e) => update({ split_duration: e.target.value })}
              />
            }
          >
            <SettingsHint>
              Split recordings into multiple VOD files once this duration is
              reached (e.g. "1h30m"). Leave empty for no duration limit. Override
              per streamer in Advanced options ("off" disables).
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Resume interrupted recordings"
            control={
              <Select
                value={cfg.resume_window || "off"}
                onValueChange={(v) => update({ resume_window: v ?? "off" })}
              >
                <SelectTrigger className="h-9 w-48">
                  <SelectValue placeholder="Resume window">
                    {(value: string | null) =>
                      resumeChoices(cfg.resume_window).find((c) => c.value === value)
                        ?.label ?? value
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {resumeChoices(cfg.resume_window).map((c) => (
                    <SelectItem key={c.value} value={c.value}>
                      {c.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            }
          >
            <SettingsHint>
              When a recording stops while the stream is still live (an app
              update, a restart or crash, or turning Record off), it continues
              in the same VOD if recording restarts within this long. The time
              missed is a gap in the VOD. A stream that ends and starts again in
              the meantime is recorded as a new VOD.
            </SettingsHint>
          </SettingsRow>
        </>
      )}
    </SettingsConfigForm>
  )
}

export default SettingsRecording

import { Input } from "@/components/ui/input"
import { SettingsRow, SettingsHint } from "@/components/settings-row"
import { SettingsConfigForm } from "@/components/settings-config-form"
import { useUserConfig } from "@/hooks/use-user-config"

// How streams are captured: the default quality and the limits that split a
// long recording into multiple VOD files.
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
        </>
      )}
    </SettingsConfigForm>
  )
}

export default SettingsRecording

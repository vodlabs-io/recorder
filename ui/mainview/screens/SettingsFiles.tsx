import { useState } from "react"
import { Link } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { Folder, Undo2 } from "lucide-react"
import { SettingsRow, SettingsHint } from "@/components/settings-row"
import { SettingsConfigForm } from "@/components/settings-config-form"
import {
  OutputTemplateInput,
  DEFAULT_OUTPUT_TEMPLATE,
} from "@/components/output-template-input"
import { useUserConfig } from "@/hooks/use-user-config"
import { getDaemonClient } from "@/lib/daemon"
import { StorageCard } from "@/components/storage-space"

// Where recordings land: the download folder, the folder layout inside it, and
// the optional archive folder they are moved to afterwards.
//
// Read-only scan folders are deliberately not edited here — this tab is about
// where recordings are written, and a list that grows with every retired disk
// would bury the two settings that decide that. They live in the VOD Archive's
// Folders popover, next to the counts and availability that say whether adding
// one worked.
export function SettingsFiles() {
  const form = useUserConfig()
  const { cfg, update, container, canChoose } = form

  // Whether the archive folder row is revealed. A saved config with a path set
  // opens it; the local override lets the box be ticked before a path is typed,
  // which no config value can represent. Unticking clears the path, so the
  // derived fallback matches again after a save.
  const [archiveShown, setArchiveShown] = useState<boolean | null>(null)
  const archiveEnabled = archiveShown ?? (cfg?.archive_dir ?? "") !== ""

  function toggleArchive(on: boolean) {
    setArchiveShown(on)
    if (!on) update({ archive_dir: "" })
  }

  async function chooseDownloadFolder() {
    const dir = await getDaemonClient().chooseDirectory(cfg?.download_dir)
    if (dir) update({ download_dir: dir })
  }

  const scanCount = cfg?.scan_dirs?.length ?? 0

  async function chooseArchiveFolder() {
    const dir = await getDaemonClient().chooseDirectory(cfg?.archive_dir)
    if (dir) update({ archive_dir: dir })
  }

  return (
    <SettingsConfigForm form={form} header={<StorageCard />}>
      {(cfg) => (
        <>
          <SettingsRow
            label="Download folder"
            control={
              canChoose || container ? (
                <p className="wrap-break-word text-sm text-muted-foreground">
                  {cfg.download_dir || "No folder selected"}
                </p>
              ) : (
                <Input
                  value={cfg.download_dir}
                  onChange={(e) => update({ download_dir: e.target.value })}
                />
              )
            }
          >
            {canChoose && (
              <Button
                type="button"
                variant="outline"
                onClick={chooseDownloadFolder}
              >
                <Folder />
                <span>Choose Folder</span>
              </Button>
            )}
            <SettingsHint>
              {container
                ? "Set by the Docker volume mounted at this path. Change it in your docker-compose configuration, not here."
                : "Where live streams are recorded to, organized into sub-folders. Keep this on a local disk for best recording performance."}
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Move recordings"
            control={
              <Checkbox
                checked={archiveEnabled}
                onCheckedChange={(v) => toggleArchive(v === true)}
              />
            }
          >
          </SettingsRow>

          {archiveEnabled && (
            <SettingsRow
              label="Archive folder"
              control={
                <div className="flex items-center gap-2">
                  <Input
                    className="flex-1"
                    placeholder="e.g. /mnt/nas/recordings"
                    value={cfg.archive_dir}
                    onChange={(e) => update({ archive_dir: e.target.value })}
                  />
                  {canChoose && (
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="h-9"
                      onClick={chooseArchiveFolder}
                    >
                      <Folder className="h-4 w-4" />
                    </Button>
                  )}
                </div>
              }
            >
              <SettingsHint>
                Finished recordings are moved here once processing completes. A separate drive or NAS is recommended.
              </SettingsHint>
              {container && (
                <SettingsHint>
                  In Docker this must be a path inside a volume you mount in
                  docker-compose.yml, e.g. add{" "}
                  <code className="font-mono text-xs">
                    - /host/path:/archive
                  </code>{" "}
                  and set this to{" "}
                  <code className="font-mono text-xs">/archive</code>. The folder
                  picker is disabled because it would pick a host path the
                  container can't write. A path that isn't writable in the
                  container is rejected on save. See docs/docker.md.
                </SettingsHint>
              )}
            </SettingsRow>
          )}

          <SettingsRow
            label="Other folders"
            control={
              <p className="text-sm text-muted-foreground">
                {scanCount === 0
                  ? "None"
                  : `${scanCount} scan folder${scanCount === 1 ? "" : "s"}`}
              </p>
            }
          >
            <SettingsHint>
              Read-only folders the VOD Archive also lists VODs from, e.g. an old
              archive on another drive, a folder of imports. Add or remove them
              in the{" "}
              <Link className="underline underline-offset-2" to="/vods">
                VOD Archive
              </Link>
              , under Folders.
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Low space warning"
            control={
              <div className="flex items-center gap-2">
                <Input
                  type="number"
                  min={0}
                  className="w-28"
                  value={cfg.low_space_warn_gb}
                  onChange={(e) =>
                    update({ low_space_warn_gb: Number(e.target.value) || 0 })
                  }
                />
                <span className="text-sm text-muted-foreground">GB</span>
              </div>
            }
          >
            <SettingsHint>
              Warn when a storage location drops below this much free space. Set to 0 to turn the warning off.
            </SettingsHint>
          </SettingsRow>

          <SettingsRow
            label="Output template"
            control={
              <div className="flex items-center gap-2">
                <OutputTemplateInput
                  className="flex-1"
                  value={cfg.output_template}
                  onChange={(v) => update({ output_template: v })}
                />
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="h-9"
                        onClick={() =>
                          update({ output_template: DEFAULT_OUTPUT_TEMPLATE })
                        }
                      >
                        <Undo2 className="h-4 w-4" />
                      </Button>
                    }
                  />
                  <TooltipContent>Reset to default</TooltipContent>
                </Tooltip>
              </div>
            }
          >
            <SettingsHint>
              {`Sub-folder layout inside the download & archive folders. Variables in curly braces:`}
              <div className="whitespace-pre font-mono text-xs">
                {`
{platform}    Platform name (e.g. TikTok)
{username}    Streamer's username
{session_id}  Unique recording session ID
{datetime}    Start date & time with timezone
{date} {time} {utc_offset} {timestamp}
{year} {month} {day} {hour} {minute} {second}`}
              </div>
            </SettingsHint>
          </SettingsRow>
        </>
      )}
    </SettingsConfigForm>
  )
}

export default SettingsFiles

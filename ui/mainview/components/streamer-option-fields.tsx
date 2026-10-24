import { useState } from "react"
import { ChevronDown, ChevronRight } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { OutputTemplateInput } from "@/components/output-template-input"

// Per-streamer overrides that can be left to inherit the global default.
export type TriState = "inherit" | "on" | "off"

const DELETE_PART_FILES_LABELS: Record<TriState, string> = {
  inherit: "Inherit global default",
  on: "Delete after processing",
  off: "Keep raw parts",
}

// The set of per-streamer recording options shared by the "Add streamer" and
// "Streamer options" dialogs. The caller owns the state; this component only
// renders the fields so both dialogs stay in lockstep. When `collapsibleAdvanced`
// is set, everything below Monitor is tucked behind an "Advanced options" toggle
// (the add flow); otherwise all fields are shown inline (the edit flow).
//
// An empty Variant/Output template means "inherit the global default"; callers
// pass that default as the *Placeholder so the field shows what it resolves to.
export function StreamerOptionFields({
  monitor,
  onMonitorChange,
  variant,
  onVariantChange,
  outputTemplate,
  onOutputTemplateChange,
  deletePartFiles,
  onDeletePartFilesChange,
  splitSize,
  onSplitSizeChange,
  splitDuration,
  onSplitDurationChange,
  archiveDir,
  onArchiveDirChange,
  variantPlaceholder = "",
  outputTemplatePlaceholder,
  splitSizePlaceholder = "Off",
  splitDurationPlaceholder = "Off",
  archiveDirPlaceholder = "Global default",
  collapsibleAdvanced = false,
  container = false,
  monitorLockedReason,
}: {
  monitor: boolean
  onMonitorChange: (value: boolean) => void
  variant: string
  onVariantChange: (value: string) => void
  outputTemplate: string
  onOutputTemplateChange: (value: string) => void
  deletePartFiles: TriState
  onDeletePartFilesChange: (value: TriState) => void
  splitSize: string
  onSplitSizeChange: (value: string) => void
  splitDuration: string
  onSplitDurationChange: (value: string) => void
  archiveDir: string
  onArchiveDirChange: (value: string) => void
  variantPlaceholder?: string
  outputTemplatePlaceholder?: string
  splitSizePlaceholder?: string
  splitDurationPlaceholder?: string
  archiveDirPlaceholder?: string
  collapsibleAdvanced?: boolean
  // When the daemon runs in Docker, an archive folder must be a container path
  // inside a mounted volume; surface that next to the field.
  container?: boolean
  // Set to lock the Monitor checkbox and say why. Switching monitoring off
  // means "record once", which the daemon only accepts for a streamer that is
  // live and recording; the edit dialog passes a reason when it isn't.
  monitorLockedReason?: string
}) {
  const [showAdvanced, setShowAdvanced] = useState(false)

  const advancedFields = (
    <>
      {/* Variant */}
      <div className="grid grid-cols-3 items-center gap-x-4 gap-y-1">
        <Label className="justify-end text-right">Variant</Label>
        <Input
          className="col-span-2"
          placeholder={variantPlaceholder}
          value={variant}
          onChange={(e) => onVariantChange(e.target.value)}
        />
        <div className="col-span-2 col-start-2 text-xs text-muted-foreground">
          Quality to record (e.g. best, worst, 720p). Empty = global default.
        </div>
      </div>

      {/* Output template */}
      <div className="grid grid-cols-3 items-center gap-x-4 gap-y-1">
        <Label className="justify-end text-right">Output template</Label>
        <OutputTemplateInput
          className="col-span-2"
          value={outputTemplate}
          onChange={onOutputTemplateChange}
          placeholder={outputTemplatePlaceholder}
        />
        <div className="col-span-2 col-start-2 text-xs text-muted-foreground">
          Per-streamer folder layout override. Empty = global default.
        </div>
      </div>

      {/* Delete part files */}
      <div className="grid grid-cols-3 items-center gap-x-4">
        <Label className="justify-end text-right">Delete part files</Label>
        <Select
          value={deletePartFiles}
          onValueChange={(v) => onDeletePartFilesChange(v as TriState)}
        >
          <SelectTrigger className="col-span-2">
            <SelectValue>
              {(value: TriState) => DELETE_PART_FILES_LABELS[value]}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="inherit">Inherit global default</SelectItem>
            <SelectItem value="on">Delete after processing</SelectItem>
            <SelectItem value="off">Keep raw parts</SelectItem>
          </SelectContent>
        </Select>
      </div>

      {/* Max file size */}
      <div className="grid grid-cols-3 items-center gap-x-4 gap-y-1">
        <Label className="justify-end text-right">Max file size</Label>
        <Input
          className="col-span-2"
          placeholder={splitSizePlaceholder}
          value={splitSize}
          onChange={(e) => onSplitSizeChange(e.target.value)}
        />
        <div className="col-span-2 col-start-2 text-xs text-muted-foreground">
          Split VOD at this raw size (e.g. "2GB"). Empty = global default; "off" = no limit.
        </div>
      </div>

      {/* Max duration */}
      <div className="grid grid-cols-3 items-center gap-x-4 gap-y-1">
        <Label className="justify-end text-right">Max duration</Label>
        <Input
          className="col-span-2"
          placeholder={splitDurationPlaceholder}
          value={splitDuration}
          onChange={(e) => onSplitDurationChange(e.target.value)}
        />
        <div className="col-span-2 col-start-2 text-xs text-muted-foreground">
          Split VOD at this duration (e.g. "1h30m"). Empty = global default; "off" = no limit.
        </div>
      </div>

      {/* Archive folder */}
      <div className="grid grid-cols-3 items-center gap-x-4 gap-y-1">
        <Label className="justify-end text-right">Archive folder</Label>
        <Input
          className="col-span-2"
          placeholder={archiveDirPlaceholder}
          value={archiveDir}
          onChange={(e) => onArchiveDirChange(e.target.value)}
        />
        <div className="col-span-2 col-start-2 text-xs text-muted-foreground">
          Move this streamer's finished recordings out of the download folder to
          here. Empty = global default; "off" = keep them in the download folder.
          {container && (
            <>
              {" "}
              In Docker, use a container path inside a mounted volume (e.g. a
              subdirectory of the archive volume like{" "}
              <code className="font-mono">/archive/name</code>); host paths are
              rejected. See the Docker guide (docs/docker.md in the recorder
              repository).
            </>
          )}
        </div>
      </div>
    </>
  )

  return (
    <>
      {/* Monitor */}
      <div className="grid grid-cols-3 items-start gap-x-4">
        <Label className="justify-end pt-0.5 text-right">Monitor</Label>
        <div className="col-span-2 flex flex-col gap-1">
          <div className="flex items-center gap-3">
            <Checkbox
              checked={monitor}
              disabled={monitorLockedReason !== undefined}
              onCheckedChange={(v) => onMonitorChange(v === true)}
            />
            <span className="text-sm text-muted-foreground">
              {monitor
                ? "Record every time streamer goes live"
                : "Record current live only (one-shot)"}
            </span>
          </div>
          {monitorLockedReason && (
            <span className="text-xs text-muted-foreground">
              {monitorLockedReason}
            </span>
          )}
        </div>
      </div>

      {collapsibleAdvanced ? (
        <div>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="px-1 text-muted-foreground"
            onClick={() => setShowAdvanced((v) => !v)}
          >
            {showAdvanced ? <ChevronDown /> : <ChevronRight />}
            Advanced options
          </Button>

          {showAdvanced && (
            <div className="mt-3 space-y-5">{advancedFields}</div>
          )}
        </div>
      ) : (
        advancedFields
      )}
    </>
  )
}

export default StreamerOptionFields

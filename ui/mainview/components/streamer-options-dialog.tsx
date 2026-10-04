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
import {
  StreamerOptionFields,
  type TriState,
} from "@/components/streamer-option-fields"
import {
  getDaemonClient,
  type StreamerView,
  type UserConfig,
} from "@/lib/daemon"
import {
  NO_MONITOR_REASON,
  platformDisplayName,
  platformNoMonitor,
} from "@/data/supported-platforms"
import { refPayload } from "@shared/daemon/subjects"
import {
  oneShotBlockedReason,
  oneShotEligible,
} from "@/screens/Dashboard/status"

export function StreamerOptionsDialog({
  streamer,
  open,
  onOpenChange,
}: {
  streamer: StreamerView | null
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const [monitor, setMonitor] = useState(true)
  const [variant, setVariant] = useState("")
  const [outputTemplate, setOutputTemplate] = useState("")
  const [deletePartFiles, setDeletePartFiles] = useState<TriState>("inherit")
  const [splitSize, setSplitSize] = useState("")
  const [splitDuration, setSplitDuration] = useState("")
  const [archiveDir, setArchiveDir] = useState("")
  // Global defaults shown as placeholders for the inherit-able fields; null
  // until loaded.
  const [defaults, setDefaults] = useState<UserConfig | null>(null)
  // A containerized daemon can only move into mounted volumes; the archive
  // folder field needs container-path guidance.
  const [container, setContainer] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Sync local form state from the streamer each time the dialog opens, and
  // (re)load the global defaults so empty fields can show what they inherit.
  useEffect(() => {
    if (open && streamer) {
      setMonitor(streamer.monitor)
      setVariant(streamer.variant)
      setOutputTemplate(streamer.outputTemplate)
      setDeletePartFiles(
        streamer.deletePartFiles === null
          ? "inherit"
          : streamer.deletePartFiles
            ? "on"
            : "off",
      )
      setSplitSize(streamer.splitSize)
      setSplitDuration(streamer.splitDuration)
      setArchiveDir(streamer.archiveDir)
      setError(null)
      getDaemonClient()
        .getConfig()
        .then((c) => {
          setDefaults(c)
          // If the streamer has no per-streamer override, seed the field with
          // the resolved global default so the user sees what they're inheriting
          // and can edit from there.
          if (!streamer.outputTemplate) setOutputTemplate(c.output_template)
        })
        .catch(() => {})
      getDaemonClient()
        .getInfo()
        .then((i) => setContainer(i.container))
        .catch(() => {})
    }
  }, [open, streamer])

  async function save() {
    if (!streamer) return
    setSaving(true)
    setError(null)
    try {
      await getDaemonClient().setStreamerOptions({
        ...refPayload(streamer),
        monitor,
        variant,
        // Send "" if the user left the field at the global default, so the
        // streamer stays on "inherit" rather than being pinned to today's value.
        output_template:
          outputTemplate.trim() === (defaults?.output_template ?? "").trim()
            ? ""
            : outputTemplate,
        delete_part_files:
          deletePartFiles === "inherit" ? null : deletePartFiles === "on",
        split_size: splitSize.trim() === "" ? null : splitSize.trim(),
        split_duration: splitDuration.trim() === "" ? null : splitDuration.trim(),
        archive_dir: archiveDir.trim() === "" ? null : archiveDir.trim(),
      })
      onOpenChange(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Streamer options</DialogTitle>
          <DialogDescription>
            {streamer
              ? `${platformDisplayName(streamer.platform)} · ${streamer.username}`
              : ""}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-5 py-2">
          <StreamerOptionFields
            monitor={monitor}
            onMonitorChange={setMonitor}
            monitorLockedReason={
              // Same rule as the dashboard's Monitor switch: only a live,
              // recording streamer can be switched to record-once, and a
              // streamer already on record-once keeps the box editable so it
              // can be promoted back, unless its site can only be recorded once.
              streamer && streamer.monitor && !oneShotEligible(streamer)
                ? oneShotBlockedReason(streamer)
                : streamer && !streamer.monitor && platformNoMonitor(streamer.platform)
                  ? NO_MONITOR_REASON
                  : undefined
            }
            variant={variant}
            onVariantChange={setVariant}
            outputTemplate={outputTemplate}
            onOutputTemplateChange={setOutputTemplate}
            deletePartFiles={deletePartFiles}
            onDeletePartFilesChange={setDeletePartFiles}
            variantPlaceholder={defaults?.variant ?? ""}
            outputTemplatePlaceholder={defaults?.output_template}
            splitSizePlaceholder={defaults?.split_size || "Off"}
            splitDurationPlaceholder={defaults?.split_duration || "Off"}
            archiveDirPlaceholder={defaults?.archive_dir || "Off"}
            splitSize={splitSize}
            onSplitSizeChange={setSplitSize}
            splitDuration={splitDuration}
            onSplitDurationChange={setSplitDuration}
            archiveDir={archiveDir}
            onArchiveDirChange={setArchiveDir}
            container={container}
          />

          {error && <p className="text-sm text-destructive">{error}</p>}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

export default StreamerOptionsDialog

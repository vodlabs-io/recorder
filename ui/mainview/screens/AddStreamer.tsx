import { useState, useRef, useEffect, type ReactElement } from "react"
import { Plus } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import {
  StreamerOptionFields,
  type TriState,
} from "@/components/streamer-option-fields"
import { appStore } from "@/state/app-state"
import { getDaemonClient, type StreamerRef, type UserConfig } from "@/lib/daemon"
import { platformDisplayName, platformNoMonitor } from "@/data/supported-platforms"

// The daemon accepts a streamer identified by either a platform + username
// combo or a URL, never both. The dialog mirrors that with a mode switcher.
type AddMode = "username" | "url"

export function AddStreamer({
  disabled,
  trigger,
}: {
  disabled?: boolean
  // Callers wanting more than the toolbar's icon-only button (the dashboard's
  // empty state wants a labelled call to action) supply their own.
  trigger?: ReactElement
}) {
  const platforms = appStore((s) => s.platforms)

  const [open, setOpen] = useState(false)
  const [mode, setMode] = useState<AddMode>("url")
  const [platform, setPlatform] = useState("")
  const [username, setUsername] = useState("")
  const [url, setUrl] = useState("")
  const [monitor, setMonitor] = useState(true)
  const [variant, setVariant] = useState("")
  const [outputTemplate, setOutputTemplate] = useState("")
  // Global defaults shown as placeholders/initial selections; null until loaded.
  const [defaults, setDefaults] = useState<UserConfig | null>(null)
  const [deletePartFiles, setDeletePartFiles] = useState<TriState>("inherit")
  const [splitSize, setSplitSize] = useState("")
  const [splitDuration, setSplitDuration] = useState("")
  const [archiveDir, setArchiveDir] = useState("")
  // A containerized daemon can only move into mounted volumes; the archive
  // folder field needs container-path guidance.
  const [container, setContainer] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Set when the daemon refused to monitor a page that can only be recorded
  // once, which the dialog then offers to do.
  const [offerOnce, setOfferOnce] = useState(false)

  const urlInputRef = useRef<HTMLInputElement>(null)
  const usernameInputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (!open) return
    const id = requestAnimationFrame(() => {
      if (mode === "url") urlInputRef.current?.focus()
      else usernameInputRef.current?.focus()
    })
    return () => cancelAnimationFrame(id)
  }, [open, mode])

  function reset() {
    setMode("url")
    setPlatform(platforms[0]?.name ?? "")
    setUsername("")
    setUrl("")
    setMonitor(true)
    setVariant("")
    setOutputTemplate("")
    setDeletePartFiles("inherit")
    setSplitSize("")
    setSplitDuration("")
    setArchiveDir("")
    setError(null)
    setOfferOnce(false)
  }

  function handleOpenChange(next: boolean) {
    setOpen(next)
    if (next) {
      reset()
      getDaemonClient()
        .getConfig()
        .then((c) => {
          setDefaults(c)
          setOutputTemplate(c.output_template)
        })
        .catch(() => {})
      getDaemonClient()
        .getInfo()
        .then((i) => setContainer(i.container))
        .catch(() => {})
    }
  }

  async function submit(monitorOverride?: boolean) {
    const monitorNow = monitorOverride ?? monitor
    // Build the streamer reference for the chosen mode. The daemon keys on
    // platform/username or url, never both, so we send exactly one.
    let ref: StreamerRef
    if (mode === "url") {
      const u = url.trim()
      if (!u) {
        setError("URL is required.")
        return
      }
      ref = { url: u }
    } else {
      const user = username.trim()
      if (!platform || !user) {
        setError("Platform and username are required.")
        return
      }
      ref = { platform, username: user }
    }
    setSubmitting(true)
    setError(null)
    setOfferOnce(false)
    try {
      const client = getDaemonClient()
      // The add answers with the identity it resolved to. Keying the options
      // call on that rather than on `ref` saves a URL add a second round trip
      // to the cloud to decode the same URL again.
      const added = await client.addStreamer({ ...ref, monitor: monitorNow })
      if (added.refused === "monitor_unsupported") {
        setError(added.message ?? "This site can only be recorded once.")
        setOfferOnce(true)
        return
      }

      // The field is pre-filled with the global default so the user can see
      // and edit it; treat "unchanged from default" as inherit so we neither
      // pin the streamer to today's value nor make a plain add look customized.
      const defaultTemplate = defaults?.output_template ?? ""
      const templateOverride =
        outputTemplate.trim() === defaultTemplate.trim() ? "" : outputTemplate

      // Only persist per-streamer options when something actually differs.
      const splitSizeTrimmed = splitSize.trim()
      const splitDurationTrimmed = splitDuration.trim()
      const archiveDirTrimmed = archiveDir.trim()
      const hasOverrides =
        !monitorNow ||
        variant.trim() !== "" ||
        templateOverride !== "" ||
        deletePartFiles !== "inherit" ||
        splitSizeTrimmed !== "" ||
        splitDurationTrimmed !== "" ||
        archiveDirTrimmed !== ""
      if (hasOverrides) {
        await client.setStreamerOptions({
          ...added,
          monitor: monitorNow,
          variant,
          output_template: templateOverride,
          delete_part_files:
            deletePartFiles === "inherit" ? null : deletePartFiles === "on",
          split_size: splitSizeTrimmed === "" ? null : splitSizeTrimmed,
          split_duration: splitDurationTrimmed === "" ? null : splitDurationTrimmed,
          archive_dir: archiveDirTrimmed === "" ? null : archiveDirTrimmed,
        })
      }
      setOpen(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          trigger ?? (
            <Button variant="outline" className="ml-auto" disabled={disabled}>
              <Plus />
            </Button>
          )
        }
      />
      <DialogContent className="max-h-[calc(100vh-2rem)] grid-rows-[1fr_auto] sm:max-w-lg">
        <Tabs
          className="min-h-0 gap-4"
          value={mode}
          onValueChange={(v) => setMode(v as AddMode)}
        >
          <DialogHeader>
            {/* Title on the left, Add-by tabs centered in the dialog on the same
                row. The 1fr / auto / 1fr columns keep the tabs centered in the
                dialog regardless of the title's width. */}
            <div className="grid grid-cols-[1fr_auto_1fr] items-center">
              <DialogTitle>Add Streamer</DialogTitle>
              <TabsList variant="line">
                <TabsTrigger value="url">URL</TabsTrigger>
                <TabsTrigger value="username">Username</TabsTrigger>
              </TabsList>
            </div>
          </DialogHeader>

          {/* Scrolls when content (e.g. expanded advanced options) exceeds the
              viewport-capped dialog height; header and footer stay fixed. The
              -mx/px pair keeps the scroll edge clear of input focus rings. */}
          <div className="-mx-1 min-h-0 flex-1 space-y-5 overflow-y-auto px-1 py-2">
            {/* Identity fields are tab-specific; the options below apply to both. */}
            <TabsContent value="url">
              {/* URL */}
              <div className="grid grid-cols-3 items-center gap-x-4">
                <Label className="justify-end text-right">URL</Label>
                <Input
                  ref={urlInputRef}
                  className="col-span-2"
                  placeholder="https://…"
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") void submit()
                  }}
                />
              </div>
            </TabsContent>

            <TabsContent value="username" className="space-y-5">
              {/* Platform */}
              <div className="grid grid-cols-3 items-center gap-x-4">
                <Label className="justify-end text-right">Platform</Label>
                <div className="col-span-2">
                  <Select<string>
                    value={platform}
                    onValueChange={(value) => setPlatform(value ?? "")}
                  >
                    <SelectTrigger className="w-50">
                      <SelectValue>
                        {(value: string) =>
                          value ? platformDisplayName(value) : "Select platform"
                        }
                      </SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      {/* A page that can only be recorded once is named by its
                          URL, never by a username. */}
                      {platforms.filter((p) => !platformNoMonitor(p.name)).map((p) => (
                        <SelectItem key={p.name} value={p.name}>
                          {platformDisplayName(p.name)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              </div>

              {/* Username */}
              <div className="grid grid-cols-3 items-center gap-x-4">
                <Label className="justify-end text-right">Username</Label>
                <Input
                  ref={usernameInputRef}
                  className="col-span-2"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") void submit()
                  }}
                />
              </div>
            </TabsContent>

            <StreamerOptionFields
              monitor={monitor}
              onMonitorChange={setMonitor}
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
              collapsibleAdvanced
            />

            {error && <AddError message={error} />}
            {offerOnce && (
              <Button
                variant="secondary"
                disabled={submitting}
                onClick={() => {
                  setMonitor(false)
                  void submit(false)
                }}
              >
                Record once
              </Button>
            )}
          </div>
        </Tabs>

        <DialogFooter>
          <DialogClose render={<Button variant="outline">Cancel</Button>} />
          {/* The add resolves the account against the cloud before it is
              accepted, so this can sit for a few seconds on a slow lookup —
              "Checking…" says what the wait is actually for. */}
          <Button onClick={() => void submit()} disabled={submitting}>
            {submitting ? "Checking…" : "Add"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// SUPPORTED_SITES_MARKER begins the sentence the cloud appends when it does not
// recognise a site at all. That sentence is guidance rather than the failure, so
// it is toned down instead of shouted in the destructive colour.
//
// Splitting on prose is safe here only because it is purely cosmetic: an
// unrecognised message renders whole, in the error colour. Nothing branches on
// it. The wording itself is composed by the cloud, which knows the platform
// list.
const SUPPORTED_SITES_MARKER = "Supported sites: "

function AddError({ message }: { message: string }) {
  const at = message.indexOf(SUPPORTED_SITES_MARKER)
  if (at === -1) {
    return <p className="text-sm text-destructive">{message}</p>
  }
  return (
    <div className="space-y-1">
      <p className="text-sm text-destructive">{message.slice(0, at).trim()}</p>
      <p className="text-muted-foreground text-xs">{message.slice(at)}</p>
    </div>
  )
}

export default AddStreamer

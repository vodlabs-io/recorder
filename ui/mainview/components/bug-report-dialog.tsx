import { useState } from "react"
import { Check, ClipboardCopy, ExternalLink } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Textarea } from "@/components/ui/textarea"
import { PrivacyLink } from "@/components/error-reports-banner"
import { getDaemonClient } from "@/lib/daemon"
import { useErrorReports } from "@/lib/error-reports"

// BugReportDialog sends what the user writes, and the recorder's recent logs
// if they leave that ticked, to VodLabs through the daemon. It works whether
// or not error reports are on: sending is the user's own choice each time.
// Opening a GitHub issue stays one click away, for a public discussion or a
// reply.
export function BugReportDialog({
  open,
  onOpenChange,
  appVersion,
  openIssue,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  appVersion?: string
  // openIssue opens the GitHub bug form, naming this report when it was sent.
  openIssue: (reportId?: string) => void
}) {
  const reports = useErrorReports()
  const [message, setMessage] = useState("")
  const [includeLogs, setIncludeLogs] = useState(true)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [sentId, setSentId] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  const reset = () => {
    setMessage("")
    setIncludeLogs(true)
    setError(null)
    setSentId(null)
    setCopied(false)
  }

  const close = (next: boolean) => {
    if (sending) return
    onOpenChange(next)
    if (!next) reset()
  }

  const send = async () => {
    setSending(true)
    setError(null)
    try {
      const web = !getDaemonClient().capabilities.installUpdates
      const res = await getDaemonClient().sendBugReport({
        message,
        include_logs: includeLogs,
        sent_from: web ? "web" : "app",
        app_version: web ? undefined : appVersion,
      })
      setSentId(res.id)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSending(false)
    }
  }

  const copyId = async () => {
    if (!sentId) return
    try {
      await navigator.clipboard.writeText(sentId)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setCopied(false)
    }
  }

  // A development build can't send; the GitHub form still works.
  const available = reports?.available !== false

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Report a bug</DialogTitle>
          <DialogDescription>
            {sentId
              ? "Thanks. The report reached VodLabs."
              : "Tell VodLabs what went wrong. Your recorder's version, client ID and operating system are sent with it."}
          </DialogDescription>
        </DialogHeader>

        {sentId ? (
          <div className="space-y-3 text-sm">
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">Report ID</span>
              <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{sentId}</code>
              <Button size="sm" variant="ghost" onClick={copyId}>
                {copied ? <Check className="h-3 w-3" /> : <ClipboardCopy className="h-3 w-3" />}
              </Button>
            </div>
            <p className="text-muted-foreground">
              Reports don't get a reply. To follow up, or to discuss it in public, open a
              GitHub issue: it will name this report.
            </p>
          </div>
        ) : available ? (
          <div className="space-y-3 text-sm">
            <Textarea
              autoFocus
              rows={6}
              placeholder="What happened, and what did you expect? Which streamer or platform, if it's about a recording."
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              disabled={sending}
            />
            <label className="flex items-start gap-2">
              <Checkbox
                className="mt-0.5"
                checked={includeLogs}
                disabled={sending}
                onCheckedChange={(v) => setIncludeLogs(v === true)}
              />
              <span>
                Attach recent logs
                <span className="block text-xs text-muted-foreground">
                  The end of each log file. Logs can name the streamers you follow, your
                  folders and the addresses the recorder connected to, with your user name,
                  emails and tokens taken out. Never your logins.
                </span>
              </span>
            </label>
            {error && <p className="text-xs text-red-500">Could not send: {error}</p>}
            <PrivacyLink />
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            This build can't send bug reports. Open a GitHub issue instead.
          </p>
        )}

        <DialogFooter className="sm:justify-between">
          <Button variant="ghost" size="sm" onClick={() => openIssue(sentId ?? undefined)}>
            Open a GitHub issue
            <ExternalLink className="ml-1 h-3 w-3" />
          </Button>
          {sentId || !available ? (
            <Button size="sm" onClick={() => close(false)}>
              Done
            </Button>
          ) : (
            <Button size="sm" disabled={sending || !message.trim()} onClick={send}>
              {sending ? "Sending…" : "Send report"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

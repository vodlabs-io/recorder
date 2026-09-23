import * as React from "react"
import { Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { getDaemonClient, type QuitPrompt } from "@/lib/daemon"

const client = getDaemonClient()

// Phase drives the dialog: "idle" hidden, "prompt" asks before quitting while
// work is in flight, "finishing" shows progress after the user confirms (the
// app exits on its own once the daemon has drained and closed).
type Phase = "idle" | "prompt" | "finishing"

function pluralize(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`
}

// describePending summarizes what stopping the daemon will interrupt or finish,
// e.g. "2 recordings in progress and 1 still saving". Shared with the Update
// tab, which warns off the same counts before an install restarts the daemon.
export function describePending({ recording, processing }: QuitPrompt): string {
  const parts: string[] = []
  if (recording > 0) parts.push(`${pluralize(recording, "recording")} in progress`)
  if (processing > 0) parts.push(`${pluralize(processing, "recording")} still saving`)
  return parts.join(" and ")
}

/**
 * QuitDialog handles the graceful-quit UX. The bun process asks to quit (via the
 * onQuitRequested push, or the getQuitPrompt pull when the window was just
 * reopened from the tray) only when the daemon has recordings/processing in
 * flight; the user can wait for it to finish or quit now. It's mounted once at
 * the app root.
 */
export function QuitDialog() {
  const [{ phase, pending }, setState] = React.useState<{
    phase: Phase
    pending: QuitPrompt | null
  }>({ phase: "idle", pending: null })

  React.useEffect(() => {
    // Only open from idle: once the user has confirmed (finishing), a late push
    // shouldn't bounce the dialog back to the prompt.
    const open = (prompt: QuitPrompt) =>
      setState((s) => (s.phase === "idle" ? { phase: "prompt", pending: prompt } : s))

    const unsubscribe = client.onQuitRequested(open)
    // Catch a prompt that fired before this just-reopened window mounted.
    client
      .getQuitPrompt()
      .then((prompt) => {
        if (prompt) open(prompt)
      })
      .catch(() => {})
    return unsubscribe
  }, [])

  function handleCancel() {
    setState({ phase: "idle", pending: null })
    client.cancelQuit().catch(() => {})
  }

  function handleConfirm() {
    setState((s) => ({ ...s, phase: "finishing" }))
    client.confirmQuit().catch(() => {})
  }

  function handleForce() {
    client.forceQuit().catch(() => {})
  }

  return (
    <AlertDialog
      open={phase !== "idle"}
      onOpenChange={(open) => {
        // Map Escape to Cancel, but only while prompting — once we're finishing,
        // shutdown is already underway and there's nothing to cancel.
        if (!open && phase === "prompt") handleCancel()
      }}
    >
      <AlertDialogContent>
        {phase === "finishing" ? (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>Finishing up…</AlertDialogTitle>
              <AlertDialogDescription>
                Saving your recordings. The app will
                close automatically when it's done.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <div className="mr-auto flex items-center text-muted-foreground text-sm">
                <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                Working…
              </div>
              <Button variant="destructive" onClick={handleForce}>
                Quit now
              </Button>
            </AlertDialogFooter>
          </>
        ) : (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>Quit VodLabs Recorder?</AlertDialogTitle>
              <AlertDialogDescription>
                {pending ? describePending(pending) + ". " : ""}
                Quitting stops recording and finishes saving your files in the
                background before the app closes.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <Button variant="outline" onClick={handleCancel}>
                Cancel
              </Button>
              <Button onClick={handleConfirm}>Quit</Button>
            </AlertDialogFooter>
          </>
        )}
      </AlertDialogContent>
    </AlertDialog>
  )
}

export default QuitDialog

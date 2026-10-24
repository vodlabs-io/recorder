import { useEffect, useState } from "react"
import { getDaemonClient, type FolderSuggestion } from "@/lib/daemon"

// The daemon's mounts don't change while it runs, so one fetch serves every
// field on the page.
let cached: Promise<FolderSuggestion[]> | null = null

// useFolderSuggestions returns the folders mounted into a container daemon,
// which are the only ones a container can keep recordings in. Empty anywhere
// else.
export function useFolderSuggestions(): FolderSuggestion[] {
  const [folders, setFolders] = useState<FolderSuggestion[]>([])
  useEffect(() => {
    let live = true
    cached ??= getDaemonClient()
      .getFolderSuggestions()
      .catch(() => {
        cached = null
        return []
      })
    void cached.then((f) => {
      if (live) setFolders(f)
    })
    return () => {
      live = false
    }
  }, [])
  return folders
}

// FolderDatalist offers the suggestions to an <input list={id}>.
export function FolderDatalist({
  id,
  folders,
  writableOnly = false,
}: {
  id: string
  folders: FolderSuggestion[]
  writableOnly?: boolean
}) {
  return (
    <datalist id={id}>
      {folders
        .filter((f) => f.writable || !writableOnly)
        .map((f) => (
          <option key={f.path} value={f.path}>
            {f.writable ? "mounted volume" : "mounted volume, read-only"}
          </option>
        ))}
    </datalist>
  )
}

// Client-local UI preferences (theme, table column visibility and page size).

export interface Preferences {
  theme: string
  dashboard_column_visibility: { [key: string]: boolean }
  dashboard_page_size: number
  // Whether the first-run welcome has been through. Stored per client rather
  // than in the daemon's config: it is about this front end having explained
  // itself, and a second machine's app should explain itself too.
  welcome_seen: boolean
}

const PREFS_KEY = "vodlabs-recorder-preferences"

export async function loadPreferences(): Promise<Partial<Preferences>> {
  const raw = localStorage.getItem(PREFS_KEY)
  if (raw) {
    try {
      return JSON.parse(raw) as Partial<Preferences>
    } catch {
      return {}
    }
  }
  return {}
}

export async function savePreferences(prefs: Preferences): Promise<void> {
  localStorage.setItem(PREFS_KEY, JSON.stringify(prefs))
}

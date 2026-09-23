import { loadPreferences, savePreferences } from "@/lib/preferences"
import { VisibilityState } from '@tanstack/react-table'
import { Preferences } from '../lib/preferences'

export interface PreferenceSlice {
    prefs: Partial<Preferences>
    prefsHydrated: boolean
    hydratePrefs: () => Promise<Preferences>
    persistPrefs: (prefs: Partial<Preferences>) => Promise<void>
}

export const DEFAULT_COLUMN_VIS: VisibilityState = {
    platform: true,
    status: true,
    last_live: false,
    vods: false,
    added: true,
    monitor: true,
    record: true,
}

export const DEFAULT_PREFS: Partial<Preferences> = {
    theme: 'system',
    dashboard_column_visibility: DEFAULT_COLUMN_VIS,
    welcome_seen: false,
}

export const createPreferenceSlice = (set: any, get: any): PreferenceSlice => ({
    prefs: DEFAULT_PREFS,
    prefsHydrated: false,
    hydratePrefs: async () => {
        if (get().prefsHydrated) return get().prefs
        try {
            const loadedPrefs = await loadPreferences()
            const mergedPrefs = { ...DEFAULT_PREFS, ...loadedPrefs }
            set({ prefs: mergedPrefs, prefsHydrated: true })
            return mergedPrefs as Preferences
        } catch (e) {
            console.error('Error loading preferences:', e)
            set({ prefs: DEFAULT_PREFS, prefsHydrated: true })
            return DEFAULT_PREFS as Preferences
        }
    },
    persistPrefs: async (prefs: Partial<Preferences>) => {
        try {
            const currentPrefs = get().prefs
            const updatedPrefs = { ...currentPrefs, ...prefs }
            set({ prefs: updatedPrefs })

            const storedPrefs = await loadPreferences()
            const mergedPrefs = { ...DEFAULT_PREFS, ...storedPrefs, ...prefs }
            await savePreferences(mergedPrefs as Preferences)
        } catch (e) {
            console.error('Error saving preferences:', e)
        }
    },
})

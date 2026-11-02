import { create } from 'zustand'
import { DaemonSlice, createDaemonSlice } from './daemon'
import { PreferenceSlice, createPreferenceSlice } from './prefs'
import { UpdateSlice, createUpdateSlice } from './update'
import { AppIntentSlice, createAppIntentSlice } from './app-intent'

export type AppStore = DaemonSlice & PreferenceSlice & UpdateSlice & AppIntentSlice

export const appStore = create<AppStore>((set, get) => ({
    ...createDaemonSlice(set, get),
    ...createPreferenceSlice(set, get),
    ...createUpdateSlice(set, get),
    ...createAppIntentSlice(set, get),
}))

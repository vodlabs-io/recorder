import { streamerChannelURL as buildChannelURL } from "@shared/channel-url"
import type { PlatformInfo } from "@shared/types"

// The daemon is the source of truth for the supported-platform list (fetched via
// the `platforms` endpoint and refreshed from the cloud heartbeat). Each entry
// carries a stylized `display_name` and a `channel_url` template, so a platform
// the cloud adds later gets a proper label and a working link without a client
// update. registerPlatforms feeds both here; DISPLAY_NAMES is the built-in
// fallback for anything shown before the list loads, and a capitalized name is
// the last resort.

const DISPLAY_NAMES: Record<string, string> = {
  tiktok: "TikTok",
  pandalive: "PandaLive",
  flextv: "FlexTV",
  bigo: "Bigo",
  twitch: "Twitch",
  kick: "Kick",
  douyin: "Douyin",
  youtube: "YouTube",
  soop: "SOOP",
  chzzk: "CHZZK",
  bilibili: "Bilibili",
  liveme: "LiveMe",
  twitcasting: "TwitCasting",
}

// Runtime labels from the daemon's platform list, keyed by lowercase name. Held
// in a module-level map (rather than passed through every call site) so
// platformDisplayName stays a plain synchronous lookup usable anywhere.
const runtimeNames: Record<string, string> = {}

// The daemon's platform list, kept so streamerChannelURL stays a plain
// synchronous lookup like platformDisplayName. There is no built-in fallback
// table of templates: the daemon stamps its own embedded default onto every
// entry it serves, so the only gap is the moment before the list has loaded —
// and no table here could be more current than the one the daemon sends.
let runtimePlatforms: PlatformInfo[] = []

// registerPlatforms records the daemon-supplied labels and live-page templates
// so later platformDisplayName / streamerChannelURL calls resolve them. Called
// whenever the platform list is (re)fetched; entries missing either field are
// left to the fallbacks.
export function registerPlatforms(platforms: PlatformInfo[]): void {
  runtimePlatforms = platforms
  for (const p of platforms) {
    if (p.display_name) runtimeNames[p.name] = p.display_name
  }
}

export function platformDisplayName(name: string): string {
  return (
    runtimeNames[name] ??
    DISPLAY_NAMES[name] ??
    (name.charAt(0).toUpperCase() + name.slice(1))
  )
}

// streamerChannelURL builds the streamer's live page on their platform from the
// registered list. See shared/channel-url for why a link has to be built at all
// and what is checked before one is returned.
export function streamerChannelURL(
  platform: string,
  username: string,
): string | null {
  return buildChannelURL(runtimePlatforms, platform, username)
}

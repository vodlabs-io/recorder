import type { PlatformInfo } from "./types";

/**
 * streamerChannelURL builds the URL of a streamer's live page on their
 * platform, or null when the platform list carries no template for it (e.g. a
 * list that has not loaded yet). No streamer page URL is stored, so this is the
 * only thing that should build one.
 *
 * The template comes from the cloud and the result is opened in the user's
 * real browser, so the scheme is checked here rather than trusted: https only.
 * The username is escaped for the same reason: it must fill one path segment.
 */
export function streamerChannelURL(
  platforms: PlatformInfo[],
  platform: string,
  username: string,
): string | null {
  if (!username) return null;
  const template = platforms.find((p) => p.name === platform)?.channel_url;
  if (!template) return null;
  const built = template.replace("{username}", encodeURIComponent(username));
  try {
    if (new URL(built).protocol !== "https:") return null;
  } catch {
    return null;
  }
  return built;
}

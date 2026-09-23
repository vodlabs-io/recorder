// How a streamer is named on screen, shared by the desktop app, the Web UI and
// the Dashboard so they all label a streamer the same way.

// streamerLabel is what to print for a streamer: the display name when the
// platform supplies one, the username otherwise. Never use it as a key, a
// filter value or a path segment — it is mutable and not unique.
export function streamerLabel(s: {
  username: string;
  displayName?: string;
}): string {
  return s.displayName || s.username;
}

// streamerInitials renders the avatar-fallback initials. Split by code point,
// not by code unit: a name starting with an emoji or any astral-plane character
// would otherwise be cut through the middle of a surrogate pair.
export function streamerInitials(label: string): string {
  return Array.from(label).slice(0, 2).join("").toUpperCase() || "?";
}

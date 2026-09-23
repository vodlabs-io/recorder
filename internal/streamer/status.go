package streamer

type LiveStatus string

const (
	StatusUnknown LiveStatus = "unknown"
	// StatusInvalid means the streamer was never verified as valid: its first
	// check said it does not exist. It is a transient answer, not a state — the
	// cloud rejects such a streamer instead of tracking it, so this arrives
	// exactly once, as the reason an add failed, and is never stored.
	StatusInvalid LiveStatus = "invalid"
	// StatusUnavailable means a streamer that WAS valid is not reachable now:
	// deleted, banned, deactivated, or withheld behind a private or age gate.
	// Platforms rarely say which, so they share one status. It is checked on a
	// slow cadence because the state is often temporary.
	StatusUnavailable LiveStatus = "unavailable"
	// StatusDefunct means the streamer stayed unavailable long enough that the
	// cloud stopped checking it. The streamer keeps its row and its place in
	// streamers.toml; only the polling ends. An explicit re-check revives it.
	StatusDefunct    LiveStatus = "defunct"
	StatusRemoved    LiveStatus = "removed"
	StatusRestricted LiveStatus = "restricted"
	// StatusAuthRequired means the room is live but only recordable with a valid
	// platform login we don't currently have (e.g. an 18+ TikTok room whose
	// stored session was rejected/expired). Returned by an authenticated
	// re-check; it drives the "sign in again" prompt in the Accounts UI.
	StatusAuthRequired LiveStatus = "auth_required"
	StatusOffline      LiveStatus = "offline"
	StatusPaused       LiveStatus = "paused"
	StatusLive         LiveStatus = "live"
)

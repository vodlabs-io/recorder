package version

// Version is overridden at build time via -ldflags "-X .../internal/version.Version=...".
// Note: the leading "v" is stripped by the goreleaser workflow.
var Version = "devel"

// Commit is the git revision this binary was built from, overridden at build
// time via -ldflags "-X .../internal/version.Commit=...". Empty when unstamped.
var Commit = ""

// Upstream contains the upstream version information.
type Upstream struct {
	// UpdateRequired is set when the service will no longer serve this version:
	// every call but the heartbeat is refused with 426 until the daemon updates.
	// It comes from the heartbeat, which answers such a client for this reason.
	UpdateRequired     bool   `json:"update_required,omitempty"`
	MinimumVersion     string `json:"minimum_version"`
	MaximumVersion     string `json:"maximum_version"`
	RecommendedVersion string `json:"recommended_version"`
	ServerVersion      string `json:"server_version,omitempty"`
	ServerStartedAt    int64  `json:"server_started_at,omitempty"` // unix seconds
}

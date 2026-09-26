package cloud

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
)

// launcherEnv names what started the daemon (desktop, cli, ...). Unset means
// the daemon was run directly.
const launcherEnv = "VLREC_LAUNCHER"

// setClientHeaders marks a request to the backend with this client's version
// and a User-Agent naming its OS, architecture and launcher. Only backend
// requests carry it: platform requests keep a browser User-Agent.
func (c *Client) setClientHeaders(req *http.Request) {
	req.Header.Set("X-Client-Version", c.clientVersion)
	req.Header.Set("User-Agent", clientUserAgent(c.clientVersion))
}

// clientUserAgent is "vodlabs-recorder/<version> (<os>; <arch>; <launcher>)".
func clientUserAgent(version string) string {
	return fmt.Sprintf("vodlabs-recorder/%s (%s; %s; %s)",
		version, runtime.GOOS, runtime.GOARCH, launcher())
}

// launcher reads launcherEnv, keeping only a short lowercase word so the
// header stays well-formed whatever the environment holds.
func launcher() string {
	v := strings.TrimSpace(os.Getenv(launcherEnv))
	if v == "" || len(v) > 16 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "standalone"
	}
	return v
}

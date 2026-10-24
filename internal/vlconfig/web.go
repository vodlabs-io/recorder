package vlconfig

// DefaultWebPort is the Web UI's HTTP listener port.
const DefaultWebPort = 8080

// WebConfig configures the HTTP listener that serves the Web UI and bridges
// the browser to the daemon's API. On by default only in a container.
type WebConfig struct {
	Enabled bool `koanf:"enabled"`
	// Host is the listener's bind address: loopback by default, every
	// interface in a container, where the port publish decides who reaches it.
	Host string `koanf:"host"`
	Port int    `koanf:"port"`
	// Token pins the sign-in token. Empty, with no TokenFile, means the token
	// generated into the config folder (see internal/webtoken).
	Token string `koanf:"token"`
	// TokenFile names a file holding the token, such as a Docker secret. It
	// wins over Token.
	TokenFile string `koanf:"token_file"`
	// AllowedOrigins are extra browser origins (scheme://host[:port]) allowed
	// to sign in and open the API socket, beyond the page's own.
	AllowedOrigins []string `koanf:"allowed_origins"`
	// TrustedProxies are the CIDRs of reverse proxies whose X-Forwarded-Proto
	// and X-Forwarded-For are believed.
	TrustedProxies []string `koanf:"trusted_proxies"`
}

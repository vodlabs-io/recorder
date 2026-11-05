package session

import (
	"net"
	"net/http"
	"time"

	"github.com/vodlabs-io/recorder/internal/netguard"
	"github.com/vodlabs-io/recorder/internal/writer/flv"
)

// The clients a local platform's streams are downloaded with. Those addresses
// come from a page, not a platform, so the clients refuse anything that is not
// public internet (a page can name a playlist, or segments, on this network),
// save the proxy the environment configures.
var (
	guardedHLSClient = &http.Client{Timeout: 10 * time.Second, Transport: guardedTransport()}
	guardedFLVClient = flv.GuardedClient()
)

func guardedTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, ControlContext: netguard.ControlContext}
	t.DialContext = dialer.DialContext
	return netguard.Transport(t)
}

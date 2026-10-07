package cloud

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/vodlabs-io/recorder/internal/netguard"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// errHostNotAllowed is returned for a scrape request aimed somewhere other than
// a supported platform.
var errHostNotAllowed = errors.New("host is not a supported platform")

// errPrivateAddress is returned when a scrape resolves to an address on this
// machine or this network.
var errPrivateAddress = netguard.ErrPrivateAddress

// scrapeMethods are the methods a scrape request may use. Checks read pages and
// call APIs; nothing about them needs to delete or overwrite anything, and a
// narrow set keeps it that way.
var scrapeMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodPost: true,
}

// checkScrapeTarget reports whether a scrape may be made for this URL.
//
// It bounds the request on the way out, before DNS: the scheme has to be
// http(s), the method has to be one checks use, and the host has to belong to a
// platform (see vlconfig.ScrapeHostAllowed). The address the host resolves to is
// checked separately, at dial time, because a name can point anywhere.
func checkScrapeTarget(method string, u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid URL")
	}
	if method == "" {
		method = http.MethodGet // http.NewRequest's own default
	}
	if !scrapeMethods[method] {
		return fmt.Errorf("method %s is not allowed for a check", method)
	}
	if !vlconfig.ScrapeHostAllowed(u.Hostname()) {
		return errHostNotAllowed
	}
	return nil
}

// newScrapeClient builds the HTTP client scrapes are made with. It is separate
// from the client the daemon talks to the service with, which legitimately
// reaches a private address when a developer runs the service locally.
//
// Each request carries its own deadline, so the client sets no overall timeout.
//
// Redirects are re-checked rather than followed blindly, so a platform URL
// cannot hand the daemon on to somewhere the guard would have refused. (Go
// already drops Cookie and Authorization headers on a cross-host redirect, so
// the login cannot travel either way.)
func newScrapeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   netguard.DialControl,
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return checkScrapeTarget(req.Method, req.URL)
		},
	}
}

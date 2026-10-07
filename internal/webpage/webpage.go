// Package webpage records what plays on a web page no platform claims, or at
// the address of an HLS or FLV stream itself, once. These are the streamers
// of the local "generic" platform (vlconfig.IsLocal): this machine finds the
// stream, and VodLabs' service never hears of the page, the stream or the
// streamer.
//
// A streamer's username is a digest of its URL (see Slug), because a page
// address is not an identity and the username must fit the shared charset and
// length. The URL itself is kept by the Resolver.
//
// A check finds the stream one of two ways:
//
//   - With the desktop app running, it loads the page in a throwaway browser
//     and reports the stream requests the page's player made (a sniff). That
//     finds streams built by JavaScript players, which is most of them.
//   - Otherwise, or when the sniff found nothing, one GET of the page and a
//     search of its HTML for playlist addresses, each probed before it is
//     believed (see static.go).
//
// SECURITY: only a URL this machine's user typed is ever loaded, and every
// fetch here, like every download of a generic recording, refuses addresses
// that are not public internet (see internal/netguard).
package webpage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vodlabs-io/recorder/internal/netguard"
)

// Platform is the generic platform's name.
const Platform = "generic"

// maxURLLength bounds a URL accepted. A page address longer than this is a
// tracking-laden share link or an attack, not something worth recording.
const maxURLLength = 2048

// slugDigestLen is how many hex digits of the URL's digest a slug carries.
const slugDigestLen = 10

// maxSlugLength is the longest username any streamer may have.
const maxSlugLength = 32

var schemePrefix = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// Canonical parses a pasted URL into the form its Slug is computed from:
// https assumed when no scheme is given, scheme and host lowercased, the
// default port and the fragment dropped, an empty path written as "/". Path
// and query are kept as they are. It refuses what can never be recorded: a
// scheme other than http(s), credentials, a host without a dot, and hosts that
// can only be this machine or a private network.
func Canonical(rawURL string) (*url.URL, error) {
	s := strings.TrimSpace(rawURL)
	if s != "" && !schemePrefix.MatchString(s) {
		s = "https://" + strings.TrimPrefix(s, "//")
	}
	if len(s) > maxURLLength {
		return nil, errors.New("URL is too long")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https URLs can be recorded")
	}
	if u.User != nil {
		return nil, errors.New("URLs with credentials cannot be recorded")
	}
	host := strings.ToLower(u.Hostname())
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return nil, errors.New("URL has no site in it")
	}
	if blockedHost(host) {
		return nil, errors.New("private and local addresses cannot be recorded")
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = host + ":" + port
	}
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	return u, nil
}

// Slug returns the identity of the URL: its host, then a digest of the whole
// canonical URL, as in "example.com-0123456789". It must never change for a
// given URL, since it names the streamer and its recordings' folder.
//
// The host is lowercased, loses a leading "www.", has anything outside
// [a-z0-9.-] mapped to "-", and is trimmed from the left so the slug fits
// maxSlugLength: whole labels while two or more would remain, then characters.
func Slug(rawURL string) (string, error) {
	u, err := Canonical(rawURL)
	if err != nil {
		return "", err
	}
	return slugOf(u), nil
}

func slugOf(u *url.URL) string {
	sum := sha256.Sum256([]byte(u.String()))
	digest := hex.EncodeToString(sum[:])[:slugDigestLen]
	return slugHost(u.Hostname()) + "-" + digest
}

func slugHost(host string) string {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	var b strings.Builder
	for _, c := range host {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	host = b.String()
	room := maxSlugLength - 1 - slugDigestLen
	for len(host) > room {
		_, rest, _ := strings.Cut(host, ".")
		if !strings.Contains(rest, ".") {
			// Dropping another label would leave only the top-level domain, so
			// cut characters instead.
			host = host[len(host)-room:]
			break
		}
		host = rest
	}
	host = strings.Trim(host, ".-")
	if host == "" {
		return "web"
	}
	return host
}

// siteName is the label for a streamer with no better one: its host.
func siteName(u *url.URL) string {
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// maxLabelRunes bounds a title or site name a page supplies.
const maxLabelRunes = 200

// cleanLabel makes text a page supplies safe to show: spaces folded and
// collapsed, control and format characters dropped, length bounded.
func cleanLabel(s string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range strings.TrimSpace(s) {
		if r == utf8.RuneError || unicode.IsControl(r) && !unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			n++
		}
		space = false
		if n >= maxLabelRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// formatOf names a stream URL's container from its address, or "" when the
// address does not say.
func formatOf(rawURL string) string {
	l := strings.ToLower(rawURL)
	switch {
	case strings.Contains(l, ".m3u8"), strings.Contains(l, "mpegurl"):
		return formatHLS
	case strings.Contains(l, ".flv"):
		return formatFLV
	}
	return ""
}

const (
	formatHLS = "hls"
	formatFLV = "flv"
)

// refererHeaders are the headers a stream found on a page is fetched with:
// most origins check the Referer, some the Origin as well.
func refererHeaders(page *url.URL) map[string]string {
	if page == nil {
		return nil
	}
	return map[string]string{
		"Referer": page.String(),
		"Origin":  page.Scheme + "://" + page.Host,
	}
}

// adHosts serve ad creatives, never the content stream. Server-side ad
// insertion (dai.google.com) is the content stream and is not listed.
var adHosts = []string{"imasdk.googleapis.com", "doubleclick.net", "googlesyndication.com"}

func isAd(host string) bool {
	host = strings.ToLower(host)
	for _, ad := range adHosts {
		if host == ad || strings.HasSuffix(host, "."+ad) {
			return true
		}
	}
	return false
}

// streamURL accepts a stream address found on a page: http(s), on a public
// host, not an ad.
func streamURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || len(raw) > maxURLLength || blockedHost(u.Hostname()) || isAd(u.Hostname()) {
		return nil, false
	}
	return u, true
}

// blockedHost is netguard.BlockedHost, replaced only by tests, whose servers
// are on loopback.
var blockedHost = netguard.BlockedHost

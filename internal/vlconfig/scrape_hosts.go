package vlconfig

import (
	"slices"
	"strings"
)

// scrapeDomains bounds where the daemon may be sent by the service.
//
// A check often has to run from the user's own address — a platform mints
// stream URLs for the IP that asks for them, and a login-gated broadcast is
// only visible to the account that holds the login — so the service hands the
// daemon a request to make and reads back the answer. This table is the answer
// to "and what else could it ask for?": the request has to land on one of these
// domains, or a subdomain of one, and anything else is refused.
//
// It is embedded and never taken from the service, for the same reason
// CookieDomains is (see withEmbeddedDefaults): a list the other end can widen
// bounds nothing at all.
//
// Keys are platform names, purely so each entry says what it is for; a request
// is checked against all of them, because most carry no platform name. A
// platform the service adds later is still checked from here until a release
// adds its domains, and its checks fall back to the service's own network
// meanwhile.
//
// Entries are registrable domains, without a leading dot.
var scrapeDomains = map[string][]string{
	"tiktok": {
		"tiktok.com",
		"tiktokcdn.com",
		"tiktokcdn-us.com",
		"tiktokcdn-eu.com",
		"tiktokv.com",
		"byteoversea.com",
	},
	"douyin": {
		"douyin.com",
		"douyinliving.com",
		"douyincdn.com",
		"douyinvod.com",
		"douyinpic.com",
		"pstatp.com",
		"snssdk.com",
	},
	"twitch": {
		"twitch.tv",
		"ttvnw.net",
		"jtvnw.net",
		"twitchcdn.net",
		// Twitch and Kick both deliver through AWS IVS.
		"live-video.net",
	},
	"youtube": {
		"youtube.com",
		"youtu.be",
		"googlevideo.com",
		"ytimg.com",
	},
	"kick": {
		"kick.com",
		"kickcdn.com",
		"live-video.net",
	},
	"pandalive": {
		"pandalive.co.kr",
		"live-video.net",
	},
	"bigo": {
		"bigo.tv",
		"bigo.sg",
		"bigolive.tv",
	},
	"soop": {
		"sooplive.com",
		"sooplive.co.kr",
		"afreecatv.com",
	},
	"chzzk": {
		"naver.com",
		"pstatic.net",
	},
	"flextv": {
		"flextv.co.kr",
		"naverncp.com",
	},
}

// scrapeHostSuffixes is scrapeDomains flattened and deduplicated, since a host
// is checked against every platform's domains.
var scrapeHostSuffixes = func() []string {
	var all []string
	for _, domains := range scrapeDomains {
		for _, d := range domains {
			d = strings.ToLower(strings.TrimPrefix(d, "."))
			if d != "" && !slices.Contains(all, d) {
				all = append(all, d)
			}
		}
	}
	slices.Sort(all)
	return all
}()

// ScrapeHostAllowed reports whether the daemon may fetch the given host on the
// service's behalf. An entry "tiktok.com" matches "tiktok.com" and any
// subdomain of it.
func ScrapeHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i] // strip port
	}
	// A trailing dot names the same host to a resolver, so it must not be a way
	// past the suffix match.
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	for _, d := range scrapeHostSuffixes {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ScrapeHostSuffixes returns the domains a scrape request may target, for
// diagnostics and tests.
func ScrapeHostSuffixes() []string { return slices.Clone(scrapeHostSuffixes) }

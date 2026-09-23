package platformauth

import (
	"strconv"
	"strings"
)

// ParseNetscapeCookies parses a Netscape/Mozilla "cookies.txt" export — the
// format every browser cookie-export extension writes, and the one curl and
// yt-dlp read. Each data line is seven tab-separated fields:
//
//	domain  include_subdomains  path  secure  expires  name  value
//
// Unlike ParseCookieHeader this keeps the domain, so a caller can drop
// everything that does not belong to the platform being signed in to: an export
// is usually the whole browser jar, and only the platform's own cookies should
// reach the daemon.
//
// Malformed lines are skipped rather than failing the parse — exports routinely
// carry blank lines, comments and trailing junk.
func ParseNetscapeCookies(raw string) []Cookie {
	var cookies []Cookie
	seen := make(map[string]bool)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// "#HttpOnly_<domain>" is a real entry; every other # line is a comment.
		httpOnly := false
		if strings.HasPrefix(line, "#") {
			const marker = "#HttpOnly_"
			if !strings.HasPrefix(line, marker) {
				continue
			}
			httpOnly = true
			line = strings.TrimPrefix(line, marker)
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		name := strings.TrimSpace(f[5])
		if name == "" {
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(f[0]))
		// Same name on two domains are different cookies; the jar is replayed
		// as one header, so keep the first and skip the duplicate.
		key := domain + "\x00" + name
		if seen[key] {
			continue
		}
		seen[key] = true

		expires, _ := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
		cookies = append(cookies, Cookie{
			Name:     name,
			Value:    strings.TrimSpace(f[6]),
			Domain:   strings.TrimPrefix(domain, "."),
			Path:     strings.TrimSpace(f[2]),
			Secure:   strings.EqualFold(strings.TrimSpace(f[3]), "TRUE"),
			HTTPOnly: httpOnly,
			Expires:  expires,
		})
	}
	return cookies
}

// CookieHeader renders cookies as a Cookie-header value ("a=1; b=2"), which is
// the shape the daemon's platformlogin.set endpoint takes.
func CookieHeader(cookies []Cookie) string {
	var b strings.Builder
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return b.String()
}

package platformauth

import "strings"

// ParseCookieHeader parses a raw Cookie-header string ("name=value; name2=v2",
// optionally with a leading "Cookie:" prefix) into cookies. It is the "advanced"
// paste path used by the Web UI and the `vod login` CLI on hosts where the
// desktop webview login isn't available. Pasted cookies carry no domain/expiry
// metadata — that's fine: the jar is replayed verbatim, and the injection guard
// keys off the platform descriptor's domains, not the cookie's own domain.
func ParseCookieHeader(raw string) []Cookie {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Tolerate a pasted "Cookie: ..." line.
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		if strings.EqualFold(strings.TrimSpace(raw[:i]), "cookie") {
			raw = raw[i+1:]
		}
	}
	var cookies []Cookie
	seen := make(map[string]bool)
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(pair[:eq])
		value := strings.TrimSpace(pair[eq+1:])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		cookies = append(cookies, Cookie{Name: name, Value: value})
	}
	return cookies
}

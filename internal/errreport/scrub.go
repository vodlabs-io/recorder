package errreport

import (
	"regexp"
	"strings"
	"sync"

	"github.com/getsentry/sentry-go"
)

const removed = "[removed]"

var (
	urlQueryRe = regexp.MustCompile(`((?:https?|wss?|nats|rtmps?)://[^\s?#"'<>]+)\?[^\s#"'<>]*`)
	emailRe    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	jwtRe      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
)

// Keys whose values are dropped whatever they hold. Matched against the
// last dotted segment of the key, lowercased.
var sensitiveKeyParts = []string{
	"cookie", "token", "authorization", "password",
	"secret", "jwt", "creds", "credential", "signature", "apikey", "api_key",
	"email",
}

var (
	homeMu   sync.RWMutex
	homeRepl *strings.Replacer
)

// setHome makes scrubString replace the user's home folder with "~", since
// its name is often the user's own. Paths in logfmt lines escape Windows
// backslashes, so that spelling is replaced too.
func setHome(home string) {
	homeMu.Lock()
	defer homeMu.Unlock()
	home = strings.TrimRight(home, `/\`)
	if len(home) < 2 {
		homeRepl = nil
		return
	}
	pairs := []string{home, "~"}
	if strings.Contains(home, `\`) {
		pairs = append(pairs, strings.ReplaceAll(home, `\`, `\\`), "~")
	}
	homeRepl = strings.NewReplacer(pairs...)
}

// scrub removes emails, tokens, URL query strings and the home folder from
// every field of ev that can carry them. IP addresses are kept.
func scrub(ev *sentry.Event) {
	// The computer's name, which Sentry would otherwise fill in.
	ev.ServerName = ""
	ev.User = sentry.User{ID: ev.User.ID, IPAddress: ev.User.IPAddress}
	ev.Request = nil
	ev.Message = scrubString(ev.Message)
	for i := range ev.Exception {
		ev.Exception[i].Type = scrubString(ev.Exception[i].Type)
		ev.Exception[i].Value = scrubString(ev.Exception[i].Value)
		if st := ev.Exception[i].Stacktrace; st != nil {
			for j := range st.Frames {
				st.Frames[j].AbsPath = scrubString(st.Frames[j].AbsPath)
			}
		}
	}
	for k, c := range ev.Contexts {
		ev.Contexts[k] = scrubMap(c)
	}
	for k, v := range ev.Tags {
		ev.Tags[k] = scrubString(v)
	}
	for _, b := range ev.Breadcrumbs {
		b.Message = scrubString(b.Message)
		b.Data = scrubMap(b.Data)
	}
}

func scrubMap(m map[string]any) map[string]any {
	for k, v := range m {
		if sensitiveKey(k) {
			m[k] = removed
			continue
		}
		m[k] = scrubValue(v)
	}
	return m
}

func scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		return scrubString(t)
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = scrubString(s)
		}
		return out
	case map[string]any:
		return scrubMap(t)
	}
	return v
}

func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = k[i+1:]
	}
	for _, p := range sensitiveKeyParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// ScrubString removes what a report must not carry from free text, such as
// a log line or the description in a bug report.
func ScrubString(s string) string { return scrubString(s) }

func scrubString(s string) string {
	if s == "" {
		return s
	}
	homeMu.RLock()
	r := homeRepl
	homeMu.RUnlock()
	if r != nil {
		s = r.Replace(s)
	}
	s = urlQueryRe.ReplaceAllString(s, "$1?"+removed)
	s = jwtRe.ReplaceAllString(s, removed)
	return emailRe.ReplaceAllString(s, "[email]")
}

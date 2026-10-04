package vlstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// A sniff permit is this machine's record that its user asked to record a
// page, which is the only thing that lets the service ask the daemon to load
// that page in a browser. The service never widens the set: only an add made
// here writes a permit.
const (
	// SniffPermitPending covers an add while the service checks the page.
	SniffPermitPending = 2 * time.Minute
	// SniffPermitRecording covers a one-shot that was accepted. Each permitted
	// sniff renews it, so a long recording keeps it.
	SniffPermitRecording = 24 * time.Hour
)

// PermitSniff records that the user asked to record pageURL, for ttl. strKey
// names the streamer the page resolved to, or is zero while that is unknown.
func PermitSniff(ctx context.Context, kv jetstream.KeyValue, pageURL string, strKey streamer.StreamerKey, ttl time.Duration) error {
	key, ok := sniffPermitKey(pageURL)
	if !ok {
		return nil
	}
	val := []byte(strKey.KVKey())
	_ = kv.Delete(ctx, key)
	_, err := kv.Create(ctx, key, val, jetstream.KeyTTL(ttl))
	return err
}

// SniffPermitted reports whether the user asked to record pageURL, and renews
// the permit of an accepted one-shot.
func SniffPermitted(ctx context.Context, kv jetstream.KeyValue, pageURL string) bool {
	if kv == nil {
		return false
	}
	key, ok := sniffPermitKey(pageURL)
	if !ok {
		return false
	}
	entry, err := kv.Get(ctx, key)
	if err != nil {
		return false
	}
	if len(entry.Value()) > 0 {
		_ = kv.Delete(ctx, key)
		_, _ = kv.Create(ctx, key, entry.Value(), jetstream.KeyTTL(SniffPermitRecording))
	}
	return true
}

// SniffedStreamer returns the streamer an accepted page's permit names.
func SniffedStreamer(ctx context.Context, kv jetstream.KeyValue, pageURL string) (streamer.StreamerKey, bool) {
	key, ok := sniffPermitKey(pageURL)
	if !ok {
		return streamer.StreamerKey{}, false
	}
	entry, err := kv.Get(ctx, key)
	if err != nil {
		return streamer.StreamerKey{}, false
	}
	return streamer.KeyFromKVKey(string(entry.Value()))
}

// RevokeSniffs removes the permits of pages that resolved to strKey.
func RevokeSniffs(ctx context.Context, kv jetstream.KeyValue, strKey streamer.StreamerKey) {
	lister, err := kv.ListKeysFiltered(ctx, "sniff.*")
	if err != nil {
		return
	}
	defer lister.Stop()
	want := strKey.KVKey()
	for key := range lister.Keys() {
		if entry, err := kv.Get(ctx, key); err == nil && string(entry.Value()) == want {
			_ = kv.Delete(ctx, key)
		}
	}
}

func sniffPermitKey(pageURL string) (string, bool) {
	c, ok := CanonicalPageURL(pageURL)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(c))
	return "sniff." + hex.EncodeToString(sum[:16]), true
}

// CanonicalPageURL is the form two spellings of one page are compared in:
// https assumed when no scheme is given, scheme and host lowercased, the
// default port and the fragment dropped, an empty path written as "/".
func CanonicalPageURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if !strings.Contains(s, "://") {
		s = "https://" + strings.TrimPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" {
		return "", false
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host += ":" + port
	}
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	return u.String(), true
}

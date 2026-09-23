// Package platformlogin publishes the NON-SECRET status of the user's
// third-party platform logins to the daemon's local KV, so the desktop and Web
// UI can render the Accounts table.
//
// PRIVACY: these docs live under the "platformlogin." KV prefix, deliberately
// OUTSIDE the "streamer.*"/"daemon.*" set the Dashboard bridge mirrors
// (internal/cloud/bridge.go), so they never reach the cloud. The docs are
// platformauth.Status values, which carry NO cookie values. Never put cookie
// material in one.
package platformlogin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/platformauth"
)

// KeyPrefix is the KV key prefix for platform-login status docs. The local UI watches
// it; the cloud bridge does not.
const KeyPrefix = "platformlogin."

const keySuffix = ".state"

// RefreshRequestSubject is the LOCAL NATS subject on which the daemon asks a
// running desktop app to refresh a platform's login: silently reload the
// platform page in the hidden login webview partition and re-capture the jar.
// Captured jars go stale (short-lived tokens stop rotating), which makes authed
// checks stop unlocking restricted rooms; only a webview can freshen them.
//
// PRIVACY: the message carries only a platform NAME. Like every
// platformlogin.* subject it is local-only — never add it (or any
// platformlogin.* subject) to the cloud bridge's allowlist.
const RefreshRequestSubject = "platformlogin.refresh"

// RefreshRequest is the payload for RefreshRequestSubject.
type RefreshRequest struct {
	Platform string `json:"platform"`
}

// Key returns the KV key for a platform's login status doc.
func Key(platform string) string { return KeyPrefix + platform + keySuffix }

// Put writes the non-secret account status doc for a platform to the local KV.
func Put(ctx context.Context, kv jetstream.KeyValue, st platformauth.Status) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, Key(st.Platform), data)
	return err
}

// Delete removes a platform's account status doc (on sign-out). A missing doc is
// not an error.
func Delete(ctx context.Context, kv jetstream.KeyValue, platform string) error {
	if err := kv.Delete(ctx, Key(platform)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// PublishAll writes a status doc for every stored login. The local KV is
// recreated on each daemon start, so this is called at startup to repopulate the
// Accounts table from the persistent cookie store.
func PublishAll(ctx context.Context, kv jetstream.KeyValue, store *platformauth.Store) error {
	for _, st := range store.List() {
		if err := Put(ctx, kv, st); err != nil {
			return err
		}
	}
	return nil
}

package streamer

import (
	"fmt"
	"strings"
)

// StreamerKey identifies a streamer by platform and username
type StreamerKey struct {
	Platform string `json:"platform,omitempty" toml:"platform,omitempty"`
	Username string `json:"username,omitempty" toml:"username,omitempty"`
}

func (k StreamerKey) Valid() bool {
	return k.Platform != "" && k.Username != ""
}

// String returns "<platform>.<encoded_username>" where dots in the username are encoded as "=".
func (k StreamerKey) String() string {
	if k.Valid() {
		return fmt.Sprintf("%s.%s", k.Platform, encodeUsername(k.Username))
	}
	return ""
}

// KVKey returns the NATS KV key for this streamer: "streamer.<platform>.<encoded_username>".
func (k StreamerKey) KVKey() string {
	if !k.Valid() {
		return ""
	}
	return "streamer." + k.String()
}

// KVSessionKey returns the NATS KV session key for this streamer:
// "streamer.<platform>.<encoded_username>.session.<session_id>".
func (k StreamerKey) KVSessionKey(sessionID string) string {
	if !k.Valid() {
		return ""
	}
	return k.KVKey() + ".session." + sessionID
}

// kVSessionKeyValid returns true if the string has the form
// "streamer.<platform>.<username>.session.<session_id>.*"
func kVSessionKeyValid(key string) bool {
	parts := strings.SplitN(key, ".", 6)
	return len(parts) == 6 &&
		parts[0] == "streamer" &&
		parts[1] != "" && // platform
		parts[2] != "" && // username
		parts[3] == "session" &&
		parts[4] != "" && // session ID
		parts[5] != ""
}

// SessionIDFromKVSessionKey extracts a sessionID from a KV session key.
func SessionIDFromKVSessionKey(key string) (string, bool) {
	if !kVSessionKeyValid(key) {
		return "", false
	}
	parts := strings.SplitN(key, ".", 6)
	return parts[4], true
}

// KeyFromKVKey parses a KV key of the form "streamer.<platform>.<encoded_username>" into a Key.
// Returns false if the key does not match the expected format.
func KeyFromKVKey(s string) (StreamerKey, bool) {
	parts := strings.Split(s, ".")
	if len(parts) < 3 || parts[0] != "streamer" || parts[1] == "" || parts[2] == "" {
		return StreamerKey{}, false
	}
	return StreamerKey{
		Platform: parts[1],
		Username: decodeUsername(parts[2]),
	}, true
}

// encodeUsername replaces "." with "=" so the username is safe to use as a NATS KV key token.
func encodeUsername(s string) string {
	return strings.ReplaceAll(s, ".", "=")
}

// decodeUsername reverses encodeUsername.
func decodeUsername(s string) string {
	return strings.ReplaceAll(s, "=", ".")
}

type StreamerKeys []StreamerKey

func (ks StreamerKeys) Strings() []string {
	strs := make([]string, len(ks))
	for i, k := range ks {
		strs[i] = k.String()
	}
	return strs
}

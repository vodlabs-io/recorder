package vlstate

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// DeleteStreamer removes every KV key for a streamer — its status, last-live,
// metadata, monitor, and session keys (everything under
// "streamer.<platform>.<username>."). Called when a streamer is unsubscribed so
// its live state doesn't linger in the KV (and resurface in clients) after it's
// removed from streamers.toml.
func DeleteStreamer(ctx context.Context, kv jetstream.KeyValue, key streamer.StreamerKey) error {
	if !key.Valid() {
		return fmt.Errorf("streamer key is invalid: %s", key)
	}
	lister, err := kv.ListKeysFiltered(ctx, key.KVKey()+".>")
	if err != nil {
		return err
	}
	defer lister.Stop()

	var firstErr error
	for kvKey := range lister.Keys() {
		if err := kv.Delete(ctx, kvKey); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

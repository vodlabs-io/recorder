package vlstate

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// oneShotTTL bounds how long a freshly-added one-shot may stay offline before
// its KV entries expire; PersistOneShot lifts it once recording starts.
const oneShotTTL = 15 * time.Second

// OneShotOptions are the per-streamer option overrides a one-shot streamer
// carries in KV once PurgeOneShots has removed it from streamers.toml. Mirrors
// the transferable subset of vlconfig's streamerOptions without importing
// vlconfig; record stays file-driven via record=false rules.
type OneShotOptions struct {
	Variant         string `json:"variant,omitempty"`
	OutputTemplate  string `json:"output_template,omitempty"`
	DeletePartFiles *bool  `json:"delete_part_files,omitempty"`
	SplitSize       string `json:"split_size,omitempty"`
	SplitDuration   string `json:"split_duration,omitempty"`
	ArchiveDir      string `json:"archive_dir,omitempty"`
}

// GetOneShotList returns streamer IDs from the KV with monitor set to false
func GetOneShotList(ctx context.Context, kv jetstream.KeyValue) streamer.StreamerRefs {
	var ids streamer.StreamerRefs

	keyLister, err := kv.ListKeysFiltered(ctx, "streamer.*.*.monitor")
	if err != nil {
		return ids
	}
	defer keyLister.Stop()

	for kvKey := range keyLister.Keys() {
		data, err := kv.Get(ctx, kvKey)
		if err != nil {
			continue
		}
		monitor := string(data.Value())

		if monitor != "false" {
			continue
		}
		strKey, ok := streamer.KeyFromKVKey(kvKey)
		if !ok {
			continue
		}
		ids = append(ids, streamer.StreamerRef{StreamerKey: strKey})
	}

	return ids
}

// PutOneShot adds the streamer to the one-shot list with a TTL, storing its
// per-streamer option overrides alongside. If the streamer is already on the
// list (its options were edited and re-purged), only the stored options are
// refreshed; .monitor and its TTL state are untouched.
func PutOneShot(ctx context.Context, kv jetstream.KeyValue, strKey streamer.StreamerKey, opts OneShotOptions) error {
	data, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	optsKey := strKey.KVKey() + ".options"
	ttl := jetstream.KeyTTL(oneShotTTL)
	if _, err := kv.Create(ctx, strKey.KVKey()+".monitor", []byte("false"), ttl); err != nil {
		_, err := kv.Put(ctx, optsKey, data)
		return err
	}
	// Fresh entry: .options shares .monitor's TTL so an expired one-shot
	// leaves nothing behind. Clear any stale revision from a previous cycle
	// first — Create requires the key to be absent.
	_ = kv.Delete(ctx, optsKey)
	_, err = kv.Create(ctx, optsKey, data, ttl)
	return err
}

// GetOneShotOptions returns the option overrides stored for a one-shot
// streamer, and whether an entry was found. Callers overlay these onto the
// file-derived options for fields the file no longer sets.
func GetOneShotOptions(ctx context.Context, kv jetstream.KeyValue, strKey streamer.StreamerKey) (OneShotOptions, bool) {
	var opts OneShotOptions
	entry, err := kv.Get(ctx, strKey.KVKey()+".options")
	if err != nil {
		return opts, false
	}
	if err := json.Unmarshal(entry.Value(), &opts); err != nil {
		return opts, false
	}
	return opts, true
}

// PersistOneShot removes the TTL for a one-shot streamer (and its stored
// options) so both survive for the duration of the recording.
func PersistOneShot(ctx context.Context, kv jetstream.KeyValue, strKey streamer.StreamerKey) error {
	optsKey := strKey.KVKey() + ".options"
	if entry, err := kv.Get(ctx, optsKey); err == nil {
		if _, err := kv.Put(ctx, optsKey, entry.Value()); err != nil {
			return err
		}
	}
	if entry, err := kv.Get(ctx, strKey.KVKey()+".monitor"); err == nil {
		monitor := string(entry.Value()) == "true"
		_, err := kv.PutString(ctx, strKey.KVKey()+".monitor", strconv.FormatBool(monitor))
		return err
	}
	return nil
}

// IsOneShot reports whether the streamer is on the KV one-shot list.
func IsOneShot(ctx context.Context, kv jetstream.KeyValue, strKey streamer.StreamerKey) bool {
	data, err := kv.Get(ctx, strKey.KVKey()+".monitor")
	return err == nil && string(data.Value()) == "false"
}

// DeleteOneShot checks whether a streamer is on the one-shot list and removes it
func DeleteOneShot(ctx context.Context, kv jetstream.KeyValue, strKey streamer.StreamerKey) (bool, error) {
	data, err := kv.Get(ctx, strKey.KVKey()+".monitor")
	if err != nil {
		return false, nil
	}
	monitor := string(data.Value())
	if monitor != "false" {
		return false, nil
	}
	_ = kv.Delete(ctx, strKey.KVKey()+".options") // best-effort; DeleteStreamer also purges it
	return true, kv.Delete(ctx, strKey.KVKey()+".monitor")
}

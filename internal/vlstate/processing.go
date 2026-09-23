package vlstate

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

func putProcessingState(
	ctx context.Context,
	kv jetstream.KeyValue,
	key streamer.StreamerKey,
	sessionID string,
	state recording.ProcessState,
) error {
	if !key.Valid() {
		return fmt.Errorf("invalid key")
	}
	_, err := kv.PutString(ctx, key.KVSessionKey(sessionID)+".proc_state", string(state))
	return err
}

func deleteProcessingState(
	ctx context.Context,
	kv jetstream.KeyValue,
	key streamer.StreamerKey,
	sessionID string,
) error {
	if !key.Valid() {
		return nil
	}
	return kv.Delete(ctx, key.KVSessionKey(sessionID)+".proc_state")
}

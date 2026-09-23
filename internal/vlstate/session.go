package vlstate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

func PutSessionStats(
	ctx context.Context,
	kv jetstream.KeyValue,
	strKey streamer.StreamerKey,
	stats recording.Stats,
) error {
	if stats == (recording.Stats{}) || stats.SessionID == "" {
		return fmt.Errorf("invalid stats")
	}
	data, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("failed to marshal stats: %w", err)
	}
	_, err = kv.Put(ctx, strKey.KVSessionKey(stats.SessionID)+".stats", data)
	return err
}

func GetSessionStats(
	ctx context.Context,
	kv jetstream.KeyValue,
	strKey streamer.StreamerKey,
	sessionID string,
) (recording.Stats, error) {
	if sessionID == "" {
		return recording.Stats{}, fmt.Errorf("empty session ID")
	}
	data, err := kv.Get(ctx, strKey.KVSessionKey(sessionID)+".stats")
	if err != nil {
		return recording.Stats{}, err
	}
	var stats recording.Stats
	if err := json.Unmarshal(data.Value(), &stats); err != nil {
		return recording.Stats{}, fmt.Errorf("failed to unmarshal stats: %w", err)
	}
	return stats, nil
}

func putSessionState(
	ctx context.Context,
	kv jetstream.KeyValue,
	strKey streamer.StreamerKey,
	sessionID string,
	state recording.SessionState,
) error {
	if !strKey.Valid() {
		return fmt.Errorf("invalid key")
	}
	if sessionID == "" {
		return fmt.Errorf("empty session ID")
	}
	_, err := kv.PutString(ctx, strKey.KVSessionKey(sessionID)+".state", string(state))
	return err
}

func GetSessionState(
	ctx context.Context,
	kv jetstream.KeyValue,
	strKey streamer.StreamerKey,
	sessionID string,
) (recording.SessionState, error) {
	if !strKey.Valid() {
		return "", fmt.Errorf("invalid key")
	}
	if sessionID == "" {
		return "", fmt.Errorf("empty session ID")
	}
	data, err := kv.Get(ctx, strKey.KVSessionKey(sessionID)+".state")
	if err != nil {
		return "", err
	}
	state := string(data.Value())
	return recording.SessionState(state), nil
}

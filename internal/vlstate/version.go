package vlstate

import (
	"context"
	"encoding/json"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/version"
)

const (
	keyDaemonVersion   = "daemon.version"
	keyUpstreamVersion = "daemon.upstream_version"
)

func PutDaemonVersion(ctx context.Context, kv jetstream.KeyValue, v string) error {
	_, err := kv.PutString(ctx, keyDaemonVersion, v)
	return err
}

func GetDaemonVersion(ctx context.Context, kv jetstream.KeyValue) (string, error) {
	entry, err := kv.Get(ctx, keyDaemonVersion)
	if err != nil {
		return "", err
	}
	return string(entry.Value()), nil
}

func PutUpstreamVersion(ctx context.Context, kv jetstream.KeyValue, u version.Upstream) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, keyUpstreamVersion, data)
	return err
}

func GetUpstreamVersion(ctx context.Context, kv jetstream.KeyValue) (version.Upstream, error) {
	entry, err := kv.Get(ctx, keyUpstreamVersion)
	if err != nil {
		return version.Upstream{}, err
	}
	var u version.Upstream
	if err := json.Unmarshal(entry.Value(), &u); err != nil {
		return version.Upstream{}, err
	}
	return u, nil
}

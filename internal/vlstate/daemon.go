package vlstate

import (
	"context"
	"strconv"

	"github.com/nats-io/nats.go/jetstream"
)

func PutCount(ctx context.Context, kv jetstream.KeyValue, key string, count int) error {
	_, err := kv.PutString(ctx, "daemon."+key, strconv.Itoa(count))
	return err
}

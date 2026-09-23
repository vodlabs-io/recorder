package vlupdate

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// WaitReady polls until the daemon's NATS server is reachable, its KV bucket is
// available, AND its API micro service is registered, or timeout elapses. The
// probe connection is closed before returning; the caller does its own connect.
func WaitReady(ctx context.Context, url, kvBucket, serviceName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	delay := 100 * time.Millisecond
	var lastErr error
	for {
		lastErr = probe(ctx, url, kvBucket, serviceName)
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon not ready at %s: %w", url, lastErr)
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func probe(ctx context.Context, url, kvBucket, serviceName string) error {
	nc, err := nats.Connect(url,
		nats.Timeout(500*time.Millisecond),
		nats.NoReconnect(),
	)
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	if _, err := js.KeyValue(ctx, kvBucket); err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := nc.RequestWithContext(pingCtx, "$SRV.PING."+serviceName, nil); err != nil {
		return err
	}
	return nil
}

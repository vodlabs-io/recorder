package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlerror"
)

// startWatchHandler receives status changes for the subscribed streamers from
// the JetStream consumer the cloud created for this subscription.
func (c *Client) startWatchHandler(ctx context.Context) error {
	log := c.logger.With("func", "startWatchHandler", "consumer", c.consumerName)

	// The cloud exposes the JetStream API under the JS.SVC prefix.
	js, err := jetstream.NewWithAPIPrefix(c.nc, "JS.SVC")
	if err != nil {
		return fmt.Errorf("failed to create JetStream context: %w", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	c.stopWatchHandler = cancel
	consumeErrCh := make(chan error, 1)

	cons, err := c.consumeMessages(watchCtx, js, consumeErrCh)
	if err != nil {
		return err
	}
	log.Info("Started consumer")

	// Restart consume loop on terminal/non-recoverable consume errors
	c.natsHandlerWg.Go(func() {
		c.manageConsumeContext(watchCtx, js, cons, consumeErrCh)
	})

	return nil
}

func (c *Client) consumeMessages(
	ctx context.Context,
	js jetstream.JetStream,
	consumeErrCh chan error,
) (jetstream.ConsumeContext, error) {
	log := c.logger.With("func", "consumeMessages", "consumer", c.consumerName)

	consumer, err := js.Consumer(ctx, "NOTIFICATIONS", c.consumerName)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %v", vlerror.ErrGetConsumerFailed, c.consumerName, err)
	}

	cons, err := consumer.Consume(func(msg jetstream.Msg) {
		data := msg.Data()
		if len(data) == 0 {
			log.Debug("No data yet", "subject", msg.Subject())
			if err := msg.Ack(); err != nil {
				log.Debug("Failed to acknowledge message", "error", err)
			}
			return
		}

		str := streamer.StreamerState{}

		if err := json.Unmarshal(data, &str); err != nil {
			log.Debug("Failed to parse streamer data", "subject", msg.Subject(), "error", err)
			if err := msg.TermWithReason("failed to parse streamer data"); err != nil {
				log.Debug("Failed to terminate message", "error", err)
			}
			return
		}
		log.Info("Received streamer update",
			"id", str.StreamerRef,
			"prev_status", str.PrevStatus,
			"status", str.Status)

		c.handleStatusUpdate(ctx, str)

		if err := msg.Ack(); err != nil {
			log.Debug("Failed to acknowledge message", "error", err)
		}
	}, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, consumeErr error) {
		if consumeErr == nil {
			return
		}
		log.Warn("JetStream consume error", "error", consumeErr)
		select {
		case consumeErrCh <- consumeErr:
		default:
		}
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to start consuming: %w", err)
	}

	return cons, nil
}

func (c *Client) manageConsumeContext(
	ctx context.Context,
	js jetstream.JetStream,
	cons jetstream.ConsumeContext,
	consumeErrCh chan error,
) {
	log := c.logger.With("func", "manageConsumeContext", "consumer", c.consumerName)
	defer log.Info("Stopped consumer")
	restartDelay := time.Second
	for {
		select {
		case <-ctx.Done():
			cons.Stop()
			return
		case <-consumeErrCh:
			cons.Stop()
			log.Warn("Restarting JetStream consumer")

			restarted := false
			for !restarted {
				select {
				case <-ctx.Done():
					return
				case <-time.After(restartDelay):
				}

				newCons, restartErr := c.consumeMessages(ctx, js, consumeErrCh)
				if restartErr != nil {
					log.Error("Failed to restart JetStream consumer", "error", restartErr)
					if errors.Is(restartErr, vlerror.ErrGetConsumerFailed) {
						log.Debug("Consumer not found, triggering resubscribe")
						c.triggerResubscribe()
						return
					}
					continue
				}

				cons = newCons
				restarted = true
				log.Info("JetStream consumer restarted")
			}
		}
	}
}

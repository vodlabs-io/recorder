package vlcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

func (a *App) recordCommand() *cli.Command {
	return &cli.Command{
		Name:      "record",
		Aliases:   []string{"rec", "r"},
		Usage:     "Start recording a streamer and watch their status live",
		ArgsUsage: streamerArgsUsage,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "once",
				Aliases: []string{"1"},
				Usage:   "Record the streamer once (don't monitor/auto-record)",
			},
			variantFlag(),
		},
		Arguments: streamerArgs(),
		Before:    a.connectDaemon,
		Action:    a.recordAction,
	}
}

func (a *App) recordAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Bool("once") {
		return a.recordOnce(ctx, cmd)
	}
	return a.addStreamerWithStatus(ctx, cmd)
}

func (a *App) addStreamerWithStatus(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "record")
	if err != nil {
		return err
	}

	variant := cmd.String("variant")
	added, err := a.sendAdd(id, variant)
	if errors.Is(err, vlapi.ErrConflict) {
		log.Printf("%s is already tracked.", describeRef(id))
		// The add carried the variant but was refused, so an already-tracked
		// streamer still needs it set the long way.
		if variant != "" {
			if err := a.sendSetVariant(id, variant); err != nil {
				return err
			}
			// A recording already running keeps the quality it started with:
			// the variant is chosen when the session opens.
			log.Printf("The new variant applies to the next recording; one already in progress keeps its current quality.")
		}
		// A refused add never says who the URL names, so there is no identity
		// to narrow the view to. Show everything rather than nothing — the
		// streamer is already tracked, so it is in there.
		return a.runStatus(ctx, cmd)
	} else if err != nil {
		return err
	}

	log.Printf("Added %s", added)
	return a.runStatusFor(ctx, cmd, added.StreamerKey)
}

// recordOnce adds a one-shot rule for the given URL and watches the
// streamer's lifecycle to a terminal outcome.
func (a *App) recordOnce(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "record --once")
	if err != nil {
		return err
	}
	added, err := a.sendRecordOnce(id, cmd.String("variant"))
	if err != nil {
		return err
	}
	log.Printf("Added %s", added)
	return a.waitForOneShotRecordable(ctx, cmd, added.StreamerKey)
}

// recordableTimeout is how long the CLI waits for a one-shot streamer to
// become recordable before returning an error.
const recordableTimeout = 15 * time.Second

// waitForOneShotRecordable waits for the streamer to become recordable before
// calling runStatus. It watches only that streamer's own keys, which is what
// the add's resolved identity buys: the URL the user typed names nothing the
// daemon stores.
func (a *App) waitForOneShotRecordable(
	ctx context.Context,
	cmd *cli.Command,
	key streamer.StreamerKey,
) error {
	if a.kv == nil {
		return fmt.Errorf("kv is not initialized")
	}
	if !key.Valid() {
		return fmt.Errorf("cannot watch an unresolved streamer")
	}

	scope := key.KVKey()
	// .monitor is the one-shot's own registration key. It matters here because
	// a one-shot that is never live gets no view doc at all: SetStatus only
	// makes a row visible once the streamer is recordable, so the doc branch
	// below cannot fire for an offline account. The registration expiring is
	// then the only signal that it never became recordable.
	watcher, err := a.kv.WatchFiltered(ctx, []string{
		scope + ".session.*.state",
		scope + ".state",
		scope + ".monitor",
	})
	if err != nil {
		return fmt.Errorf("failed to watch kv keys: %w", err)
	}
	defer watcher.Stop()

	// Whether the doc has been seen; a later delete of it means the streamer
	// was removed while we were waiting.
	docSeen := false
	// Whether the one-shot registration has been seen, so its later removal can
	// be told apart from the initial watch replay.
	monitorSeen := false

	pendingTimer := time.NewTimer(recordableTimeout)
	defer pendingTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-pendingTimer.C:
			return a.oneShotGaveUp(ctx, key)

		case entry, ok := <-watcher.Updates():
			if !ok {
				return nil
			}
			if entry == nil {
				continue
			}
			kvKey := entry.Key()
			switch {
			case strings.HasSuffix(kvKey, ".monitor"):
				if entry.Operation() == jetstream.KeyValuePut {
					monitorSeen = true
					continue
				}
				if monitorSeen {
					// The registration expired or was removed before the
					// streamer ever became recordable.
					return a.oneShotGaveUp(ctx, key)
				}
			case vlstate.IsDocKVKey(kvKey):
				if entry.Operation() != jetstream.KeyValuePut {
					if docSeen {
						return fmt.Errorf("streamer is %s", streamer.StatusRemoved)
					}
					continue
				}
				var doc vlstate.StreamerDoc
				if err := json.Unmarshal(entry.Value(), &doc); err != nil {
					continue
				}
				docSeen = true
				switch doc.Status {
				case streamer.StatusInvalid,
					streamer.StatusUnavailable,
					streamer.StatusDefunct,
					streamer.StatusRemoved,
					streamer.StatusRestricted,
					streamer.StatusOffline:
					// An account that is not there is as final as an offline one:
					// say so now rather than waiting out the timeout for a stream
					// that cannot start.
					return fmt.Errorf("streamer is %s", doc.Status)
				default:
					log.Printf("Streamer is %s", doc.Status)
				}
			case strings.Contains(kvKey, ".session."):
				state := recording.SessionState(string(entry.Value()))
				switch state {
				case recording.SessionRecording, recording.SessionLagging, recording.SessionPaused:
					return a.runStatusFor(ctx, cmd, key)
				}
			}
		}
	}
}

// oneShotGaveUp explains a one-shot that never started recording. The status is
// read best-effort: an account that was live-then-offline has a view doc to
// quote, while one that was offline the whole time has none, because a one-shot
// only earns a doc once it is recordable.
func (a *App) oneShotGaveUp(ctx context.Context, key streamer.StreamerKey) error {
	if doc, err := vlstate.GetStreamerDoc(ctx, a.kv, key); err == nil && doc.Status != "" {
		return fmt.Errorf("%s is %s — nothing to record", key, doc.Status)
	}
	return fmt.Errorf(
		"%s did not go live within %s, so nothing was recorded.\n"+
			"A one-shot only waits that long. To record them whenever they next go live, run '%s record %s %s'",
		key, recordableTimeout, RootCommandName, key.Platform, key.Username)
}

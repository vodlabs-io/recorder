package vlcli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/progress"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

func (a *App) statusCommand() *cli.Command {
	return &cli.Command{
		Name:   "status",
		Usage:  "View current daemon status with real-time recording statistics",
		Before: a.connectDaemon,
		Commands: []*cli.Command{
			a.statusFilterCommand(),
		},
		Action: a.runStatus,
	}
}

func (a *App) statusFilterCommand() *cli.Command {
	return &cli.Command{
		Name:    "filter",
		Aliases: []string{"fil", "f"},
		Usage:   "Filter status entries by platform, username, or both",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "platform", Aliases: []string{"p"},
				Usage: "Only show entries for this platform"},
			&cli.StringFlag{Name: "username", Aliases: []string{"u"},
				Usage: "Only show entries for this streamer"},
		},
		Action: a.runStatus,
	}
}

func (a *App) runStatus(ctx context.Context, cmd *cli.Command) error {
	// Guarded only when `status` was named: the root action shares this path and
	// has already vetted its own arguments.
	if cmd.Name == "status" {
		if err := rejectExtraArgs(cmd, 0, "status"); err != nil {
			return err
		}
	}
	return a.runStatusFor(ctx, cmd, streamer.StreamerKey{})
}

// runStatusFor renders the live status view, narrowed to `only` when it names a
// streamer and to the --platform/--username flags otherwise.
//
// Callers that just added a streamer pass the identity the add resolved to,
// rather than the URL the user typed: a URL is not stored anywhere, so it can
// match nothing here.
func (a *App) runStatusFor(
	ctx context.Context,
	cmd *cli.Command,
	only streamer.StreamerKey,
) error {
	if a.kv == nil {
		return fmt.Errorf("kv is not initialized")
	}

	platform := cmd.String("platform")
	username := cmd.String("username")
	encodedUsername := strings.ReplaceAll(username, ".", "=")
	filtered := true

	streamerScope := "streamer.*.*"
	switch {
	case only.Valid():
		streamerScope = only.KVKey()
	case platform != "" && username != "":
		streamerScope = fmt.Sprintf("streamer.%s.%s", platform, encodedUsername)
	case platform != "":
		streamerScope = fmt.Sprintf("streamer.%s.*", platform)
	case username != "":
		streamerScope = fmt.Sprintf("streamer.*.%s", encodedUsername)
	default:
		filtered = false
	}

	startTime := time.Now()
	if entry, err := a.kv.Get(ctx, "daemon.start_time"); err == nil {
		if ts, err := strconv.ParseInt(string(entry.Value()), 10, 64); err == nil {
			startTime = time.Unix(ts, 0)
		}
	}

	filters := []string{
		"daemon.subscription_count",
		"daemon.worker_count",
		streamerScope + ".monitor",
		streamerScope + ".session.*.state",
		streamerScope + ".session.*.stats",
		streamerScope + ".session.*.proc_state",
	}
	watcher, err := a.kv.WatchFiltered(ctx, filters)
	if err != nil {
		return fmt.Errorf("failed to watch kv keys: %w", err)
	}
	defer watcher.Stop()

	recordingRows := map[string]*progress.StreamerRow{}
	processingRows := map[string]*progress.StreamerRow{}
	subscriptionCount := 0
	workerCount := 0

	display := &progress.MultiLineDisplay{}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case entry, ok := <-watcher.Updates():
			if !ok {
				return nil
			}
			if entry == nil {
				continue
			}
			kvKey := entry.Key()
			kvKeySuffix := kvKey[strings.LastIndex(kvKey, ".")+1:]

			if kvKey == "daemon.subscription_count" {
				if n, err := strconv.Atoi(string(entry.Value())); err == nil {
					subscriptionCount = n
				}
				continue
			}
			if kvKey == "daemon.worker_count" {
				if n, err := strconv.Atoi(string(entry.Value())); err == nil {
					workerCount = n
				}
				continue
			}
			strKey, ok := streamer.KeyFromKVKey(kvKey)
			if !ok {
				continue
			}
			// One-shot streamer reached a terminal state
			if cmd.Bool("once") &&
				kvKeySuffix == "monitor" &&
				entry.Operation() == jetstream.KeyValueDelete {
				return nil
			}
			sessionID, ok := streamer.SessionIDFromKVSessionKey(kvKey)
			if !ok {
				continue
			}

			switch kvKeySuffix {
			case "state":
				switch entry.Operation() {
				case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
					delete(recordingRows, sessionID)
					continue
				}
				sessionState := recording.SessionState(string(entry.Value()))
				switch sessionState {
				case recording.SessionRecording, recording.SessionLagging, recording.SessionPaused:
					upsertRow(recordingRows, sessionID, strKey).Label = string(sessionState)
				case recording.SessionCancelled, recording.SessionInvalid, recording.SessionRestricted,
					recording.SessionFailed, recording.SessionFinished:
					delete(recordingRows, sessionID)
				}

			case "stats":
				if entry.Operation() != jetstream.KeyValuePut {
					continue
				}
				var stats recording.Stats
				if err := json.Unmarshal(entry.Value(), &stats); err != nil {
					continue
				}
				upsertRow(recordingRows, sessionID, strKey).Stats = stats

			case "proc_state":
				switch entry.Operation() {
				case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
					delete(processingRows, sessionID)
					continue
				}
				stats, err := vlstate.GetSessionStats(ctx, a.kv, strKey, sessionID)
				if err != nil {
					continue
				}
				finishedLabel := "proc_done"

				switch recording.ProcessState(string(entry.Value())) {
				case recording.ProcessProcessing:
					r := upsertRow(processingRows, sessionID, strKey)
					r.Label = string(recording.ProcessProcessing)
					r.Stats = stats
					continue
				case recording.ProcessRejected:
					continue
				case recording.ProcessCancelled:
					finishedLabel = "proc_stop"
				case recording.ProcessFailed:
					finishedLabel = "proc_fail"
				case recording.ProcessFinished:
					if state, err := vlstate.GetSessionState(ctx, a.kv, strKey, sessionID); err == nil {
						finishedLabel = string(state)
					}
				}
				_, exists := processingRows[sessionID]
				if exists {
					display.PrintFinishedRow(finishedLabel, stats, strKey.Platform, strKey.Username)
				}
				delete(processingRows, sessionID)
			}

		case <-ticker.C:
			displayRows := make([]progress.StreamerRow, 0, len(recordingRows)+len(processingRows))
			for _, r := range recordingRows {
				if r.Label == "" {
					// No session_state yet — skip until we know what to label it.
					continue
				}
				displayRows = append(displayRows, *r)
			}
			for _, r := range processingRows {
				displayRows = append(displayRows, *r)
			}
			display.Render(progress.DisplayState{
				Uptime:            time.Since(startTime),
				RecordingCount:    workerCount,
				SubscriptionCount: subscriptionCount,
				Filtered:          filtered,
				Rows:              displayRows,
			})
		}
	}
}

// upsertRow returns the row for the given session ID, creating it if necessary.
func upsertRow(m map[string]*progress.StreamerRow, sessionID string, k streamer.StreamerKey) *progress.StreamerRow {
	r, ok := m[sessionID]
	if !ok {
		r = &progress.StreamerRow{Platform: k.Platform, Username: k.Username}
		m[sessionID] = r
	}
	return r
}

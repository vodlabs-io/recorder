package session

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/vodlabs-io/recorder/internal/flvfix"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlerror"
)

// workerState represents the current state of the session worker
type workerState int

const (
	statusReceived workerState = iota
	sessionCancelled
	streamerOffline
	streamLagging
	streamPaused
	streamRecording
)

// reconnectPlan decides how the next attempt is made after a part that ended
// with err and recorded partBytes. A part that could not reconnect asks for a
// fresh URL, since the platform may serve the stream from another server by
// now. If it was recording up to the drop, the stream is most likely still
// live and the lag delay is skipped: every second spent waiting is footage
// lost. An attempt that fails again records nothing, so it waits as usual.
func reconnectPlan(err error, partBytes int64) (freshURL, skipDelay bool) {
	if !errors.Is(err, vlerror.ErrConnectFailed) {
		return false, false
	}
	return true, partBytes > 0
}

// unmonitoredStallLimit is how long a session on a no_monitor platform may go
// without data before it ends. Nothing reports such a streamer offline, so a
// stream that ended while its last check still read live would otherwise keep
// the session lagging forever.
const unmonitoredStallLimit = 3 * time.Minute

// endsUnmonitored reports whether a session that has recorded nothing for gap
// should end because its platform is never monitored.
func endsUnmonitored(platform string, gap time.Duration) bool {
	return gap >= unmonitoredStallLimit && vlconfig.NoMonitor(platform)
}

// streamHost returns the server a stream URL points at, for logging: the rest
// of the URL can carry signing tokens.
func streamHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Run records the Session
func (w *worker) Run(ctx context.Context) error {
	defer w.finishSession()
	// Writer state outliving a Part — the FLV stream held open across a
	// codec-configuration rotation — is released however the session ends.
	defer w.carryover.Close()
	// Stopped before finishSession runs (defers unwind in reverse), so the
	// final reading it takes is the last word on the sidecar.
	metaDone := make(chan struct{})
	defer close(metaDone)
	go w.runMetadataLog(metaDone)

	const shortLagTimeout = 1 * time.Minute
	const shortLagDelay = 5 * time.Second
	const longLagDelay = 30 * time.Second
	const lagDelayJitter = 3 * time.Second
	const lagReportInterval = 2 * time.Minute
	var lastLagReport time.Time
	// The attempt a re-resolve was already asked for, so one failed attempt
	// asks once.
	refreshedAtAttempt := -1
	// How the last part that did not rotate wants the next attempt made.
	var refreshAfterPart, skipLagDelay bool
	var lagStart time.Time
	sessionStart := time.Now()
	state := statusReceived

	for {
		if ctx.Err() != nil {
			state = sessionCancelled
		}

		switch state {
		case statusReceived:
			str := w.str.Load()
			if str == nil {
				return errors.New("streamer is nil")
			}
			w.logger.Info("Received status", "status", str.Status)
			switch str.Status {
			case streamer.StatusUnknown:
				return errors.New("status is unknown")
			case streamer.StatusInvalid:
				return errors.New("streamer is invalid")
			case streamer.StatusUnavailable, streamer.StatusDefunct:
				// The account went away underneath a running session — banned,
				// deleted or switched private mid-stream. There is nothing left
				// to record, and unlike a transient failure there is no point
				// retrying.
				return errors.New("streamer account is unavailable")
			case streamer.StatusRestricted:
				return errors.New("stream is restricted")
			case streamer.StatusOffline:
				state = streamerOffline
				continue
			case streamer.StatusPaused:
				state = streamPaused
			case streamer.StatusLive:
				if w.recAttempts > 0 {
					state = streamLagging
				} else {
					state = streamRecording
				}
			default:
				return fmt.Errorf("unrecognized status: %s", str.Status)
			}
			if str.Status == streamer.StatusLive || w.selectedStream == nil {
				if err := w.initStream(*str); err != nil {
					return fmt.Errorf("failed to initialize stream: %w", err)
				}
			}
		case sessionCancelled:
			w.logger.Info("Session cancelled")
			if w.output.Stats.TotalBytes > 0 {
				w.logFinalStats(sessionStart)
			}
			return nil
		case streamerOffline:
			if w.output.Stats.TotalBytes > 0 {
				w.logger.Info("Stream has ended")
				w.logFinalStats(sessionStart)
				return nil
			} else {
				return fmt.Errorf("streamer is offline")
			}
		case streamLagging, streamPaused:
			if state == streamPaused {
				w.store.SetSessionState(ctx, w.strKey, w.SessionID, recording.SessionPaused)
			}
			lagDelay := longLagDelay
			if time.Since(lagStart) < shortLagTimeout {
				lagDelay = addJitter(shortLagDelay, lagDelayJitter)
			}
			if state == streamLagging && skipLagDelay {
				lagDelay = 0
			}
			select {
			case <-ctx.Done():
				state = sessionCancelled
				continue
			case <-w.notifyCh:
				state = statusReceived
				continue
			case <-time.After(lagDelay):
			}
			if state == streamLagging {
				// A part ends for reasons that say nothing about the stream — a
				// playlist poll out of retries, a codec change, a split limit —
				// and usually resumes within seconds. A lag is a hole in the
				// recording, so the gap since the last byte is what decides.
				gap := w.dataGap(sessionStart)
				if endsUnmonitored(w.strKey.Platform, gap) {
					w.logger.Info("No data for too long on a platform nothing monitors; treating the stream as ended",
						"idle", gap.Round(time.Second))
					state = streamerOffline
					continue
				}
				if gap >= stallTimeout {
					w.logger.Info("Stream is lagging", "idle", gap.Round(time.Second))
					w.store.SetSessionState(ctx, w.strKey, w.SessionID, recording.SessionLagging)
					if time.Since(lastLagReport) > lagReportInterval {
						w.reportLag()
						lastLagReport = time.Now()
						w.logger.Info("Lag reported")
					}
				}
				// A hole in the recording points at the stream URL as readily as
				// at the stream, and a retry on a URL the platform has stopped
				// honouring can never succeed: ask for a freshly signed one and
				// wait for it here. Some platforms sign a URL for only a few
				// tens of seconds, so the reply is applied and recorded immediately.
				needsFreshURL := refreshAfterPart || w.lastRecordingWasEmpty() || gap >= stallTimeout
				if needsFreshURL && refreshedAtAttempt != w.recAttempts {
					w.requestFreshStreams()
					refreshedAtAttempt = w.recAttempts
					w.logger.Info("Requested fresh stream URLs")
					select {
					case <-ctx.Done():
						state = sessionCancelled
						continue
					case <-w.notifyCh:
						str := w.str.Load()
						if str == nil || str.Status != streamer.StatusLive {
							state = statusReceived
							continue
						}
						if err := w.initStream(*str); err != nil {
							return fmt.Errorf("failed to initialize stream: %w", err)
						}
						w.logger.Info("Applied fresh stream URLs", "host", streamHost(w.selectedStream.URL))
					case <-time.After(streamRefreshWait):
					}
				}
			}
			state = streamRecording
		case streamRecording:
			if err := w.initPart(); err != nil {
				return fmt.Errorf("failed to initialize part: %w", err)
			}
			wLogger := w.logger.With("writer", w.writer.Name())
			recordCtx, cancelRecord := context.WithCancel(ctx)
			w.output.CurrentPart.StartTime = time.Now()
			// One channel per part, so no increment outlives the part that
			// earned it.
			progressCh := make(chan int64, progressBufSize)
			progressDone := make(chan struct{})
			go w.listenProgress(recordCtx, cancelRecord, w.output.CurrentPart.StartTime, progressCh, progressDone)
			w.logger.Info("Recording started", "part_index", w.output.PartIndex)
			err := w.writer.Record(
				recordCtx,
				w.curPart,
				w.writerCfg,
				w.selectedHTTPClient,
				wLogger,
				progressCh)
			w.output.CurrentPart.EndTime = time.Now()
			w.recAttempts++
			cancelRecord() // Ensure recording context is cancelled
			// Record has returned, so no sender remains: closing ends the
			// listener, and waiting for it settles this part's byte total
			// before the MinPartBytes guard below reads it.
			close(progressCh)
			<-progressDone
			// Consume the pending split atomically; nil if none was triggered.
			splitKey := w.splitPending.Swap(nil)
			// A codec configuration change ends the part cleanly; the next
			// part records the new config and opens a group of its own.
			configChange := errors.Is(err, flvfix.ErrConfigChange)
			refreshAfterPart, skipLagDelay = reconnectPlan(err, w.output.CurrentPart.Bytes)
			if errors.Is(err, vlerror.ErrWriteFailed) {
				return err
			}
			if w.output.CurrentPart.Bytes >= recording.MinPartBytes {
				w.logger.Info("Part completed",
					"part_index", w.output.PartIndex,
					"part_bytes", w.output.CurrentPart.Bytes,
					"part_duration_ms", time.Since(w.output.CurrentPart.StartTime).Milliseconds(),
					"output_file", w.output.CurrentPart.Path)
				// Record the finished part (now carrying its StartTime/EndTime/
				// Bytes) so the manifest and DB-queue persistence see real times.
				w.output.Parts = append(w.output.Parts, w.output.CurrentPart)
				w.output.PartIndex++
				lagStart = time.Now()
				// Fold the part into its config's group (enqueues its FLV
				// convert).
				w.recordGroupPart()
			} else {
				if w.output.CurrentPart.Bytes > 0 {
					w.logger.Info("Part size below threshold",
						"part_index", w.output.PartIndex,
						"part_bytes", w.output.CurrentPart.Bytes,
						"min_part_bytes", recording.MinPartBytes)
				} else {
					w.logger.Info("No data recorded for part", "part_index", w.output.PartIndex)
				}
				if err := w.deleteEmptyPart(); err != nil {
					w.logger.Warn("Failed to delete empty part", "error", err)
				}
			}
			// Emit the group that reached its limit — not necessarily the one
			// this part joined, since a part's config key only arrives as it
			// finishes. A part dropped above joins no group, but the group the
			// split named is over its limit either way.
			if splitKey != nil {
				w.closeGroup(*splitKey, false)
			}
			// After a split or config change the stream is still live: loop
			// straight back into recording without the lag delay.
			if (splitKey != nil || configChange) && ctx.Err() == nil {
				w.logger.Info("Resuming recording after part rotation",
					"next_part_index", w.output.PartIndex,
					"config_change", configChange)
				continue
			}
			state = streamLagging
		}
	}
}

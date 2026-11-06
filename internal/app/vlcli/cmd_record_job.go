package vlcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// Outcomes of an unattended `vod record --once`.
const (
	outcomeRecorded    = "recorded"
	outcomeNotLive     = "not_live"
	outcomeUnavailable = "unavailable"
	outcomeFailed      = "failed"
	outcomeUnreachable = "unreachable"
	outcomeError       = "error"
)

// exitNotLive separates "nothing to record yet" from a failure, so a caller can
// retry later without reading the output.
const exitNotLive = 2

const (
	finalizeTimeout    = 10 * time.Minute
	interruptedTimeout = 30 * time.Second
	missingVODGrace    = 45 * time.Second
	finalizePoll       = 2 * time.Second
	progressInterval   = time.Minute
)

// checkDelays space out the job's own status checks. The first waits for the
// daemon to subscribe to the streamer, which the service needs before it checks.
var checkDelays = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}

// recordResult is the single report of an unattended recording.
type recordResult struct {
	Outcome     string          `json:"outcome"`
	Platform    string          `json:"platform,omitempty"`
	Username    string          `json:"username,omitempty"`
	LiveStatus  string          `json:"live_status,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	State       string          `json:"state,omitempty"`
	Stopped     bool            `json:"stopped,omitempty"` // by --duration or a signal
	Ready       bool            `json:"ready"`             // files are final
	Dir         string          `json:"dir,omitempty"`
	Paths       []string        `json:"paths,omitempty"`
	Files       []vodindex.File `json:"files,omitempty"`
	TotalBytes  int64           `json:"total_bytes,omitempty"`
	DurationSec float64         `json:"duration_sec,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// recordJob records one broadcast without a person watching: it waits for the
// stream, optionally stops it after a set time, waits for the files to be
// final, and reports once.
type recordJob struct {
	app      *App
	ref      streamer.StreamerRef
	variant  string
	wait     time.Duration
	duration time.Duration

	// Overridden by tests.
	finalizeTimeout    time.Duration
	interruptedTimeout time.Duration
	missingVODGrace    time.Duration
	finalizePoll       time.Duration
	checkDelays        []time.Duration
}

func (a *App) runRecordJob(ctx context.Context, cmd *cli.Command) error {
	ref, err := streamerRefFromArgs(cmd, "record --once")
	if err != nil {
		return err
	}
	wait, dur := cmd.Duration("wait"), cmd.Duration("duration")
	if wait < 0 || dur < 0 {
		return errors.New("--wait and --duration cannot be negative")
	}
	if wait == 0 {
		wait = recordableTimeout
	}
	job := &recordJob{
		app: a, ref: ref, variant: cmd.String("variant"), wait: wait, duration: dur,
		finalizeTimeout:    finalizeTimeout,
		interruptedTimeout: interruptedTimeout,
		missingVODGrace:    missingVODGrace,
		finalizePoll:       finalizePoll,
		checkDelays:        checkDelays,
	}
	res := job.run(ctx)
	if cmd.Bool("json") {
		if err := emitJSON(res); err != nil {
			return err
		}
		return exitFor(res, "")
	}
	for _, p := range res.Paths {
		fmt.Println(p)
	}
	return exitFor(res, res.summary())
}

func exitFor(res recordResult, msg string) error {
	switch res.Outcome {
	case outcomeRecorded:
		if msg != "" {
			log.Print(msg)
		}
		return nil
	case outcomeNotLive:
		return cli.Exit(msg, exitNotLive)
	default:
		return cli.Exit(msg, 1)
	}
}

func (r recordResult) summary() string {
	who := r.Platform + "." + r.Username
	switch r.Outcome {
	case outcomeRecorded:
		if !r.Ready {
			return fmt.Sprintf("Recorded %s, but its files are still being processed in %s", who, r.Dir)
		}
		return fmt.Sprintf("Recorded %s: %s, %s", who,
			shortDuration(r.DurationSec), humanBytes(uint64(max(r.TotalBytes, 0))))
	case outcomeNotLive:
		return fmt.Sprintf("%s did not go live in time, so nothing was recorded", who)
	case outcomeUnavailable:
		return fmt.Sprintf("%s is %s, so nothing was recorded", who, r.LiveStatus)
	case outcomeUnreachable:
		return "This machine cannot reach the platform, so nothing was recorded: " + r.Error
	default:
		return "Error: " + r.Error
	}
}

// run reports every outcome through its result, so --json always gets one.
func (j *recordJob) run(ctx context.Context) recordResult {
	var res recordResult
	ref, temporary, err := j.add()
	if err != nil {
		res.Outcome, res.Error = outcomeError, err.Error()
		return res
	}
	key := ref.StreamerKey
	if !key.Valid() {
		res.Outcome, res.Error = outcomeError, "cannot watch an unresolved streamer"
		return res
	}
	res.Platform, res.Username = key.Platform, key.Username
	log.Printf("Added %s", key)

	// Checks run while the job waits find out early whether this machine can
	// reach the platform at all, rather than after the whole wait.
	checkCtx, stopChecks := context.WithCancel(ctx)
	checked := j.checkUntilAnswered(checkCtx, key)
	end := j.watch(ctx, key, temporary, checked, &res)
	stopChecks()

	started := res.SessionID != ""
	if temporary || end.stop || !started {
		// Ending the rule cancels a running session, and its files still get
		// processed. A finished one-shot has already cleaned up after itself.
		if _, err := vlapi.Request[streamer.StreamerRef, string](
			j.app.nc, "streamer.remove", streamer.StreamerRef{StreamerKey: key}); err != nil {
			log.Printf("Could not stop tracking %s: %v. Run 'vod streamer remove %s %s'.",
				key, err, key.Platform, key.Username)
		}
	}
	if end.outcome != "" {
		res.Outcome, res.Error = end.outcome, end.err
		return res
	}
	res.Stopped = end.stop

	timeout := j.finalizeTimeout
	if ctx.Err() != nil {
		timeout = j.interruptedTimeout
	}
	found := j.finalize(key, timeout, &res)

	completed := end.stop || res.State == string(recording.SessionFinished) || res.State == ""
	switch {
	case !completed:
		res.Outcome = outcomeFailed
		res.Error = "the recording ended as " + res.State
	case !found:
		res.Outcome = outcomeFailed
		res.Error = "the recording produced no files"
	default:
		res.Outcome = outcomeRecorded
	}
	return res
}

// checkUntilAnswered runs status checks on the job's schedule until one answers
// or says the platform is unreachable, and reports the last result.
func (j *recordJob) checkUntilAnswered(ctx context.Context, key streamer.StreamerKey) <-chan error {
	out := make(chan error, 1)
	go func() {
		var err error
		for _, d := range j.checkDelays {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			_, err = j.app.checkNow(streamer.StreamerRef{StreamerKey: key})
			if err == nil || errors.Is(err, vlapi.ErrPlatformUnreachable) {
				break
			}
		}
		out <- err
	}()
	return out
}

// add registers the streamer. A wait longer than a one-shot's lifetime needs a
// monitored rule, which run removes again; temporary reports that case.
func (j *recordJob) add() (streamer.StreamerRef, bool, error) {
	if j.wait > recordableTimeout {
		ref, err := j.app.sendAdd(j.ref, j.variant)
		if !errors.Is(err, vlapi.ErrMonitorUnsupported) {
			return ref, err == nil, j.withConflictHint(err)
		}
		log.Print("This page cannot be monitored, so it has to be live now.")
	}
	ref, err := j.app.sendRecordOnce(j.ref, j.variant)
	return ref, false, j.withConflictHint(err)
}

func (j *recordJob) withConflictHint(err error) error {
	if !errors.Is(err, vlapi.ErrConflict) {
		return err
	}
	return fmt.Errorf("%w\nIts recordings are listed by 'vod vods list'; to run this command instead, first run 'vod streamer remove %s'",
		err, describeRef(j.ref))
}

// watchEnd is why watching stopped. An empty outcome means a session ran and
// the caller should collect its files.
type watchEnd struct {
	outcome string
	stop    bool // the recording has to be stopped
	err     string
}

// watch follows the streamer until its session ends, the wait or duration runs
// out, or ctx is cancelled. It records progress in res.
func (j *recordJob) watch(
	ctx context.Context,
	key streamer.StreamerKey,
	temporary bool,
	checked <-chan error,
	res *recordResult,
) watchEnd {
	scope := key.KVKey()
	watcher, err := j.app.kv.WatchFiltered(ctx, []string{
		scope + ".session.*.state",
		scope + ".session.*.stats",
		scope + ".state",
		scope + ".monitor",
	})
	if err != nil {
		return watchEnd{outcome: outcomeError, err: fmt.Sprintf("failed to watch kv keys: %v", err)}
	}
	defer watcher.Stop()

	waitTimer := time.NewTimer(j.wait)
	defer waitTimer.Stop()
	var durationC <-chan time.Time
	progress := time.NewTicker(progressInterval)
	defer progress.Stop()

	replayed, monitorSeen := false, false
	var stats recording.Stats

	interrupted := func() watchEnd {
		if res.SessionID == "" {
			return watchEnd{outcome: outcomeError, err: "interrupted before the stream went live"}
		}
		log.Print("Interrupted; stopping the recording.")
		return watchEnd{stop: true}
	}

	for {
		select {
		case <-ctx.Done():
			return interrupted()

		case <-waitTimer.C:
			if res.SessionID == "" {
				return watchEnd{outcome: outcomeNotLive}
			}

		case <-durationC:
			log.Printf("Reached --duration; stopping the recording.")
			return watchEnd{stop: true}

		case err := <-checked:
			checked = nil
			switch {
			case err == nil:
			case errors.Is(err, vlapi.ErrPlatformUnreachable):
				if res.SessionID == "" {
					return watchEnd{outcome: outcomeUnreachable,
						err: strings.TrimPrefix(err.Error(), vlapi.ErrPlatformUnreachable.Error()+": ")}
				}
			default:
				log.Printf("Status check failed: %v", err)
			}

		case <-progress.C:
			if res.SessionID != "" {
				log.Printf("Recording %s: %s, %s", key, shortDuration(stats.Duration.Seconds()),
					humanBytes(uint64(max(stats.TotalBytes, 0))))
			}

		case entry, ok := <-watcher.Updates():
			if !ok {
				if ctx.Err() != nil {
					return interrupted()
				}
				if res.SessionID == "" {
					return watchEnd{outcome: outcomeError, err: "lost contact with the daemon"}
				}
				return watchEnd{stop: true}
			}
			if entry == nil {
				replayed = true
				continue
			}
			kvKey := entry.Key()
			put := entry.Operation() == jetstream.KeyValuePut

			switch {
			case strings.HasSuffix(kvKey, ".monitor"):
				if put {
					monitorSeen = true
				} else if monitorSeen && !temporary && res.SessionID == "" {
					return watchEnd{outcome: outcomeNotLive}
				}

			case vlstate.IsDocKVKey(kvKey):
				var doc vlstate.StreamerDoc
				if !put || json.Unmarshal(entry.Value(), &doc) != nil {
					continue
				}
				res.LiveStatus = string(doc.Status)
				if res.SessionID == "" {
					if outcome := outcomeForStatus(doc.Status, temporary); outcome != "" {
						return watchEnd{outcome: outcome}
					}
				}

			case strings.HasSuffix(kvKey, ".stats"):
				var s recording.Stats
				if put && json.Unmarshal(entry.Value(), &s) == nil {
					stats = s
				}

			default:
				sessionID, ok := streamer.SessionIDFromKVSessionKey(kvKey)
				if !ok {
					continue
				}
				if !put {
					if sessionID == res.SessionID {
						return watchEnd{} // purged with its streamer
					}
					continue
				}
				switch state := recording.SessionState(entry.Value()); state {
				case recording.SessionRecording, recording.SessionLagging, recording.SessionPaused:
					if res.SessionID == "" {
						res.SessionID = sessionID
						log.Printf("Recording %s (session %s)", key, sessionID)
						if j.duration > 0 {
							durationC = time.After(j.duration)
						}
					}
				default:
					// A replayed end state belongs to an earlier session.
					if sessionID == res.SessionID || (replayed && res.SessionID == "") {
						res.SessionID = sessionID
						res.State = string(state)
						return watchEnd{}
					}
				}
			}
		}
	}
}

// outcomeForStatus names a live status that ends the wait. A monitored wait
// sits through a streamer being offline or briefly restricted, which a one-shot
// has no time for.
func outcomeForStatus(status streamer.LiveStatus, temporary bool) string {
	switch status {
	case streamer.StatusInvalid, streamer.StatusDefunct, streamer.StatusRemoved:
		return outcomeUnavailable
	case streamer.StatusUnavailable, streamer.StatusRestricted:
		if !temporary {
			return outcomeUnavailable
		}
	case streamer.StatusOffline:
		if !temporary {
			return outcomeNotLive
		}
	}
	return ""
}

// finalize waits for the session's files to be ready and reports whether the
// session has any. The archive is the only record left once a stopped session's
// state has been purged.
func (j *recordJob) finalize(key streamer.StreamerKey, timeout time.Duration, res *recordResult) bool {
	start := time.Now()
	found := false
	for {
		page, err := vlapi.Request[vodListRequest, vodListPage](j.app.nc, "vods.list",
			vodListRequest{Limit: vodListFetchLimit, Rescan: true})
		if err != nil {
			log.Printf("Could not list recordings: %v", err)
		} else {
			for _, v := range page.Sessions {
				if v.SessionID != res.SessionID {
					continue
				}
				found = true
				res.fill(v)
				if v.State == vodindex.StateReady {
					res.Ready = true
					return true
				}
			}
		}
		elapsed := time.Since(start)
		if elapsed >= timeout || (!found && elapsed >= j.missingVODGrace) {
			return found
		}
		time.Sleep(j.finalizePoll)
	}
}

func (r *recordResult) fill(v vodindex.VOD) {
	r.Dir = v.Dir
	r.Files = v.Files
	r.TotalBytes = v.TotalBytes
	r.DurationSec = v.DurationSec
	r.Paths = r.Paths[:0]
	for _, f := range v.Files {
		r.Paths = append(r.Paths, filepath.Join(v.Dir, f.Name))
	}
}

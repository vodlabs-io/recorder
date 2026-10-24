package vlcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vlupdate"
)

func (a *App) connectDaemon(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	ctx, cancel := context.WithCancel(ctx)
	var err error

	a.nc, a.kv, err = initNATS(ctx, a.natsURL, cancel, a.closing.Load)
	if err == nil {
		a.notifyDaemonUpdate(ctx, cmd)
		return ctx, nil
	}

	if !daemonDown(err) || !a.cfg.AutoStart || version.Version == "devel" {
		cancel()
		return ctx, a.daemonUnreachableError(err)
	}

	if err := a.autoStartDaemon(ctx, cmd.String("config"), version.Version); err != nil {
		cancel()
		return ctx, err
	}

	a.nc, a.kv, err = initNATS(ctx, a.natsURL, cancel, a.closing.Load)
	if err != nil {
		cancel()
		return ctx, a.daemonUnreachableError(err)
	}

	a.notifyDaemonUpdate(ctx, cmd)
	return ctx, nil
}

func (a *App) notifyDaemonUpdate(ctx context.Context, cmd *cli.Command) {
	if cmd.Name == "update" {
		return
	}
	daemonVer, err := vlstate.GetDaemonVersion(ctx, a.kv)
	if err != nil {
		return
	}
	current, err := semver.NewVersion(daemonVer)
	if err != nil {
		return
	}
	upstream, err := vlstate.GetUpstreamVersion(ctx, a.kv)
	if err != nil {
		return
	}
	recommended, err := semver.NewVersion(upstream.RecommendedVersion)
	if err != nil {
		return
	}
	if !current.LessThan(recommended) {
		return
	}
	fmt.Fprintf(os.Stderr,
		"A new version is available: %s (current: %s). Run '%s update' to update.\n",
		recommended, current, RootCommandName)
}

// daemonUnreachableError phrases a failed connect in terms of the recorder
// rather than its transport. "nats: no servers available for connection" names
// the transport three times and the remedy none; a user needs to know the
// daemon is not running and how to start it. The underlying error is still
// wrapped, so errors.Is still works and the detail is one -v away.
func (a *App) daemonUnreachableError(err error) error {
	if !daemonDown(err) {
		return fmt.Errorf("could not reach the recorder at %s: %w", a.natsURL, err)
	}
	hint := fmt.Sprintf("start it with 'vlrecd', or set daemon.auto_start = true in %s",
		vlconfig.DefaultConfigFilePath())
	if vlconfig.RunningInContainer() {
		hint = "it may still be starting, or it stopped; check 'docker compose logs recorder' on the host"
	} else if a.cfg.AutoStart {
		hint = "it did not come up; check the log above or run 'vlrecd' directly to see why"
	}
	return fmt.Errorf("the recorder daemon isn't running at %s — %s: %w", a.natsURL, hint, err)
}

// daemonDown reports whether a failed connect means nothing is listening.
// nats.go folds a refused dial into ErrNoServers only when the message reads
// "connection refused", which Windows words differently, so the errno is
// checked too.
func daemonDown(err error) bool {
	return errors.Is(err, nats.ErrNoServers) || isConnRefused(err)
}

const natsKVBucket = "streamers"

func initNATS(
	ctx context.Context,
	url string,
	onClose context.CancelFunc,
	isClosing func() bool,
) (*nats.Conn, jetstream.KeyValue, error) {
	nc, err := nats.Connect(url,
		nats.NoReconnect(),
		nats.ClosedHandler(func(_ *nats.Conn) {
			if !isClosing() {
				fmt.Println(cr() + "Daemon disconnected")
				onClose()
			}
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to NATS at %s: %w", url, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	kv, err := js.KeyValue(ctx, natsKVBucket)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open KV bucket %q: %w", natsKVBucket, err)
	}
	return nc, kv, nil
}

func (a *App) autoStartDaemon(ctx context.Context, cfgFile, ver string) error {
	exePath, err := vlupdate.Locate(ctx, a.cfg.ExePath, ver)
	if errors.Is(err, vlupdate.ErrNotFound) {
		cacheDir, cerr := vlupdate.CacheDir(ver)
		if cerr != nil {
			return cerr
		}
		exePath, err = vlupdate.Download(ctx, a.cfg.BinaryRepo, ver, cacheDir, vlupdate.DaemonArtifact())
	}
	if err != nil {
		return fmt.Errorf("locating daemon binary: %w", err)
	}

	logPath := vlconfig.DefaultCacheFilePath(spawnLogName)
	if err := vlupdate.Spawn(ctx, exePath, cfgFile, logPath); err != nil {
		return fmt.Errorf("spawning daemon: %w", err)
	}
	if err := vlupdate.WaitReady(ctx, a.natsURL, natsKVBucket, vlapi.ServiceName, 10*time.Second); err != nil {
		// The daemon's own words are the whole diagnosis — "bind: address
		// already in use" versus a bare "not ready" is the difference between a
		// one-line fix and a mystery.
		if tail := vlupdate.SpawnLogTail(logPath, spawnLogTailBytes); tail != "" {
			return fmt.Errorf("%w\n\nThe daemon exited during startup:\n%s\n\nFull log: %s",
				err, indentLines(tail, "  "), logPath)
		}
		return fmt.Errorf("%w (no output in %s)", err, logPath)
	}
	return nil
}

// spawnLogName is where an auto-started daemon's stdout and stderr land, so a
// daemon that dies before it can serve still says why.
const spawnLogName = "daemon-spawn.log"

// spawnLogTailBytes bounds how much of that log is quoted into an error.
const spawnLogTailBytes = 4096

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

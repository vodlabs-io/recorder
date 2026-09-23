package vlcli

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) quitCommand() *cli.Command {
	return &cli.Command{
		Name:   "quit",
		Usage:  "Quit the daemon",
		Flags:  []cli.Flag{forceFlag()},
		Before: a.connectDaemon,
		Action: a.runQuit,
	}
}

func (a *App) restartCommand() *cli.Command {
	return &cli.Command{
		Name:   "restart",
		Usage:  "Restart the daemon",
		Flags:  []cli.Flag{forceFlag()},
		Before: a.connectDaemon,
		Action: a.runRestart,
	}
}

func forceFlag() cli.Flag {
	return &cli.BoolFlag{
		Name:    "force",
		Aliases: []string{"f"},
		Usage:   "Skip post-processing of pending recordings",
	}
}

func shutdownSubject(force bool) string {
	if force {
		return "shutdown_force"
	}
	return "shutdown"
}

func (a *App) runQuit(ctx context.Context, cmd *cli.Command) error {
	_, err := vlapi.Request[any, string](a.nc, shutdownSubject(cmd.Bool("force")), nil)
	if err != nil {
		return err
	}
	a.waitForDaemonExit(ctx)
	log.Println("Daemon stopped.")
	return nil
}

func (a *App) runRestart(ctx context.Context, cmd *cli.Command) error {
	// In a container the daemon's lifecycle is owned by Docker, not this CLI:
	// shutting it down would let the container exit (or be restarted by Docker)
	// rather than respawning the daemon on the host. Tell the user how to
	// restart the container instead.
	info, err := vlapi.Request[any, vlapi.DaemonInfo](a.nc, "info", nil)
	if err != nil {
		return err
	}
	if info.Container {
		log.Println("The daemon is running in a Docker container; 'vod restart' has no effect.")
		log.Println("Restart the container instead with:\n  docker compose restart")
		return nil
	}

	return a.restartDaemon(ctx, cmd.String("config"), version.Version, cmd.Bool("force"))
}

func (a *App) restartDaemon(ctx context.Context, cfgFile, ver string, force bool) error {
	_, err := vlapi.Request[any, string](a.nc, shutdownSubject(force), nil)
	if err != nil {
		return err
	}

	a.waitForDaemonExit(ctx)
	a.nc = nil

	log.Printf("Starting daemon...")
	if err := a.autoStartDaemon(ctx, cfgFile, ver); err != nil {
		return err
	}
	log.Println("Daemon restarted successfully.")
	return nil
}

// waitForDaemonExit waits for the daemon's NATS connection to close. If ctx is
// cancelled (user pressed Ctrl+C) before the daemon exits, it sends a
// shutdown_force request and continues waiting until the daemon is gone.
func (a *App) waitForDaemonExit(ctx context.Context) {
	var once sync.Once
	daemonGone := make(chan struct{})

	a.nc.SetClosedHandler(func(_ *nats.Conn) {
		once.Do(func() { close(daemonGone) })
	})

	hint := time.AfterFunc(500*time.Millisecond, func() {
		log.Println("Processing pending recordings. Press Ctrl+C to cancel and exit immediately.")
	})

	select {
	case <-daemonGone:
		hint.Stop()
		return
	case <-ctx.Done():
		hint.Stop()
	}

	log.Println(cr() + "Cancelling processing...")
	if _, err := vlapi.Request[any, string](a.nc, "shutdown_force", nil); err != nil {
		log.Printf("Force shutdown request failed: %v", err)
	}
	<-daemonGone
}

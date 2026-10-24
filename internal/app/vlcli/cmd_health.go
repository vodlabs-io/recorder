package vlcli

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

// healthTimeout bounds each probe, well inside a container healthcheck's own
// timeout.
const healthTimeout = 3 * time.Second

func (a *App) healthCommand() *cli.Command {
	return &cli.Command{
		Name:  "health",
		Usage: "Exit 0 if the daemon is up and answering, 1 if not (never starts one)",
		Description: "Pings the daemon's API service. Meant for container healthchecks " +
			"and scripts; it never auto-starts a daemon.",
		Action: a.runHealth,
	}
}

func (a *App) runHealth(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "health"); err != nil {
		return err
	}
	if err := pingDaemon(a.natsURL); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// pingDaemon checks that the daemon's API service answers, not just that its
// NATS port accepts connections.
func pingDaemon(url string) error {
	nc, err := nats.Connect(url, nats.NoReconnect(), nats.Timeout(healthTimeout))
	if err != nil {
		return fmt.Errorf("daemon unreachable at %s: %w", url, err)
	}
	defer nc.Close()
	if _, err := nc.Request("$SRV.PING."+vlapi.ServiceName, nil, healthTimeout); err != nil {
		return fmt.Errorf("daemon at %s did not answer: %w", url, err)
	}
	return nil
}

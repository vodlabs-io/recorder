package vlcli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// healthTimeout bounds each probe, well inside a container healthcheck's own
// timeout.
const healthTimeout = 3 * time.Second

func (a *App) healthCommand() *cli.Command {
	return &cli.Command{
		Name:  "health",
		Usage: "Exit 0 if the daemon is up and answering, 1 if not (never starts one)",
		Description: "Pings the daemon's API service, and the Web UI when it is enabled. " +
			"Meant for container healthchecks and scripts; it never auto-starts a daemon.",
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
	if a.web.Enabled {
		if err := checkWebUI(ctx, webLocalURL(a.web)+"healthz"); err != nil {
			return err
		}
	}
	fmt.Println("ok")
	return nil
}

// checkWebUI checks that the Web UI answers its health route.
func checkWebUI(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("web UI unreachable at %s: %w", url, err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("web UI at %s answered %s", url, res.Status)
	}
	return nil
}

// webLocalURL is the Web UI's address from this machine, with a trailing
// slash.
func webLocalURL(cfg vlconfig.WebConfig) string {
	host := cfg.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Port)) + "/"
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

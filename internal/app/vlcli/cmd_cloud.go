package vlcli

import (
	"context"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) cloudCommand() *cli.Command {
	return &cli.Command{
		Name:   "cloud",
		Usage:  "Per-platform polling intervals reported by the VodLabs service",
		Before: a.connectDaemon,
		Flags:  []cli.Flag{jsonFlag()},
		Action: a.runCloud,
	}
}

func (a *App) runCloud(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "cloud"); err != nil {
		return err
	}
	res, err := vlapi.Request[any, vlapi.CloudIntervals](a.nc, "cloud.intervals", nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return emitJSON(res)
	}

	if res.TargetSeconds > 0 {
		fmt.Printf("Target interval: %s\n\n", formatSeconds(res.TargetSeconds))
	}

	tw := newTable()
	fmt.Fprintln(tw, "PLATFORM\tINTERVAL\tUPDATED")
	for _, pi := range res.Intervals {
		interval := "unknown"
		if pi.EstimatedSeconds != nil {
			interval = formatSeconds(*pi.EstimatedSeconds)
		}
		updated := "—"
		if pi.UpdatedAt != nil {
			updated = formatAgo(time.Since(time.Unix(*pi.UpdatedAt, 0)))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", pi.Platform, interval, updated)
	}
	return tw.Flush()
}

// formatSeconds renders a whole number of seconds as a compact duration (e.g. "4m32s").
func formatSeconds(sec int) string {
	return (time.Duration(sec) * time.Second).String()
}

// formatAgo renders elapsed time as "<duration> ago".
func formatAgo(d time.Duration) string {
	if d < time.Second {
		return "just now"
	}
	return d.Round(time.Second).String() + " ago"
}

package vlcli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) configErrorReportsCommand() *cli.Command {
	return &cli.Command{
		Name:      "error-reports",
		Usage:     "Show, or turn on or off, sending error reports to VodLabs",
		ArgsUsage: "[on|off]",
		Description: "Error reports tell VodLabs when the recorder logs an error or crashes, so it can be fixed. " +
			"They are off until you turn them on. What a report holds: https://vodlabs.io/privacy#error-reports",
		Action: a.runConfigErrorReports,
	}
}

func (a *App) runConfigErrorReports(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 1, "config error-reports"); err != nil {
		return err
	}
	var state *vlapi.ErrorReportsState
	var err error
	switch arg := strings.ToLower(cmd.Args().First()); arg {
	case "":
		state, err = vlapi.Request[any, vlapi.ErrorReportsState](a.nc, "reports.get", nil)
	case "on", "off":
		state, err = vlapi.Request[map[string]bool, vlapi.ErrorReportsState](a.nc, "reports.set",
			map[string]bool{"enabled": arg == "on"})
	default:
		return fmt.Errorf("expected on or off, not %q", arg)
	}
	if err != nil {
		return err
	}
	switch {
	case !state.Available:
		fmt.Println("Error reports: not available in this build")
	case state.Enabled == nil:
		fmt.Println("Error reports: off (not set; turn on with 'vod config error-reports on')")
	case *state.Enabled:
		fmt.Println("Error reports: on")
	default:
		fmt.Println("Error reports: off")
	}
	return nil
}

func (a *App) reportCommand() *cli.Command {
	return &cli.Command{
		Name:      "report",
		Usage:     "Send a bug report to VodLabs",
		ArgsUsage: "<what went wrong>",
		Description: "Sends your description, the recorder's version and client ID, and your OS to VodLabs. " +
			"With --logs it also attaches the end of each log file, which can name the streamers you follow " +
			"and your folders. Works whether or not error reports are on. " +
			"What a report holds: https://vodlabs.io/privacy#error-reports",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "logs", Usage: "attach the recorder's recent logs"},
		},
		Before: a.connectDaemon,
		Action: a.runReport,
	}
}

func (a *App) runReport(ctx context.Context, cmd *cli.Command) error {
	msg := strings.TrimSpace(strings.Join(cmd.Args().Slice(), " "))
	if msg == "" {
		return fmt.Errorf("describe the problem, e.g. vod report \"recordings of <streamer> stop after an hour\"")
	}
	res, err := vlapi.Request[vlapi.BugReportRequest, vlapi.BugReportResult](a.nc, "reports.bug",
		vlapi.BugReportRequest{Message: msg, IncludeLogs: cmd.Bool("logs"), SentFrom: "cli"})
	if err != nil {
		return err
	}
	fmt.Printf("Sent. Report ID: %s\n", res.ID)
	fmt.Println("Quote this ID if you also open an issue at https://github.com/vodlabs-io/recorder/issues.")
	return nil
}

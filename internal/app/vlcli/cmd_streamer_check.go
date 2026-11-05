package vlcli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) streamerCheckCommand() *cli.Command {
	return &cli.Command{
		Name:      "check",
		Usage:     "Check a streamer's status now and print it, or why the check failed",
		ArgsUsage: streamerArgsUsage,
		Arguments: streamerArgs(),
		Action:    a.checkStreamer,
	}
}

func (a *App) checkStreamer(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "streamer check")
	if err != nil {
		return err
	}
	status, err := a.checkNow(id)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", describeRef(id), status)
	return nil
}

// checkNow runs a status check and waits for it. A daemon that cannot wait, or
// a streamer whose check needs a login, answers "checking" and checks in the
// background instead.
func (a *App) checkNow(id streamer.StreamerRef) (string, error) {
	res, err := vlapi.Request[vlapi.CheckRequest, string](a.nc, "streamer.check",
		vlapi.CheckRequest{StreamerRef: id, Wait: true})
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	return *res, nil
}

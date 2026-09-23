package vlcli

import (
	"context"

	"github.com/urfave/cli/v3"
)

func (a *App) streamerCheckCommand() *cli.Command {
	return &cli.Command{
		Name:      "check",
		Usage:     "Check and update a streamer's status on demand",
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
	return a.sendStreamerRequest("streamer.check", id)
}

package vlcli

import (
	"context"

	"github.com/urfave/cli/v3"
)

func (a *App) streamerRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Aliases:   []string{"rm"},
		Usage:     "Stop tracking a streamer",
		ArgsUsage: streamerArgsUsage,
		Arguments: streamerArgs(),
		Action:    a.removeStreamer,
	}
}

func (a *App) removeStreamer(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "streamer remove")
	if err != nil {
		return err
	}
	return a.sendStreamerRequest("streamer.remove", id)
}

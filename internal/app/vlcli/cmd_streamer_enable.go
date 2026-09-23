package vlcli

import (
	"context"

	"github.com/urfave/cli/v3"
)

func (a *App) streamerDisableCommand() *cli.Command {
	return &cli.Command{
		Name:      "disable",
		Usage:     "Disable recording for a streamer (keeps monitoring it)",
		ArgsUsage: streamerArgsUsage,
		Arguments: streamerArgs(),
		Action:    a.disableStreamer,
	}
}

func (a *App) streamerEnableCommand() *cli.Command {
	return &cli.Command{
		Name:      "enable",
		Usage:     "Enable recording for a streamer",
		ArgsUsage: streamerArgsUsage,
		Arguments: streamerArgs(),
		Action:    a.enableStreamer,
	}
}

func (a *App) disableStreamer(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "streamer disable")
	if err != nil {
		return err
	}
	return a.sendStreamerRequest("streamer.record_off", id)
}

func (a *App) enableStreamer(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "streamer enable")
	if err != nil {
		return err
	}
	return a.sendStreamerRequest("streamer.record_on", id)
}

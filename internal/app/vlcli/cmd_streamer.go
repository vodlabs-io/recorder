package vlcli

import (
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) streamerCommand() *cli.Command {
	return &cli.Command{
		Name:    "streamer",
		Aliases: []string{"str"},
		Usage:   "Add, list and configure the streamers being tracked",
		Before:  a.connectDaemon,
		Commands: []*cli.Command{
			a.streamerListCommand(),
			a.streamerAddCommand(),
			a.streamerRemoveCommand(),
			a.streamerDisableCommand(),
			a.streamerEnableCommand(),
			a.streamerCheckCommand(),
			a.streamerVariantCommand(),
			a.streamerConfigCommand(),
		},
	}
}

// sendStreamerRequest sends id to subject and prints what the daemon said, so
// a reply like "already recording" reaches the user.
func (a *App) sendStreamerRequest(subject string, id streamer.StreamerRef) error {
	res, err := vlapi.Request[streamer.StreamerRef, string](a.nc, subject, id)
	if err != nil {
		return err
	}
	if res != nil && *res != "" {
		fmt.Printf("%s: %s\n", describeRef(id), *res)
	}
	return nil
}

// describeRef labels a streamer for output. A URL is echoed as typed; there is
// nothing else to show until the daemon resolves it.
func describeRef(id streamer.StreamerRef) string {
	if s := id.String(); s != "" {
		return s
	}
	return id.URL
}

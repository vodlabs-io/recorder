package vlcli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// platformsCommand lists the platforms the daemon can record. The supported set
// comes from the cloud and changes without a client release, so it has to be
// asked for rather than documented in help text.
func (a *App) platformsCommand() *cli.Command {
	return &cli.Command{
		Name:   "platforms",
		Usage:  "List the streaming sites this recorder supports",
		Before: a.connectDaemon,
		Flags:  []cli.Flag{jsonFlag()},
		Action: a.runPlatforms,
	}
}

func (a *App) runPlatforms(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "platforms"); err != nil {
		return err
	}
	res, err := vlapi.Request[any, []vlconfig.Platform](a.nc, "platforms", nil)
	if err != nil {
		return err
	}
	list := *res
	if cmd.Bool("json") {
		return emitJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("No platforms reported by the daemon.")
		return nil
	}

	w := newTable()
	fmt.Fprintln(w, "PLATFORM\tNAME\tLOGIN\tEXAMPLE URL")
	for _, p := range list {
		login := "-"
		if p.Login != nil {
			login = "supported"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			p.Name, dash(p.DisplayName), login, dash(p.ChannelURL))
	}
	return w.Flush()
}

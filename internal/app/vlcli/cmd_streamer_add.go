package vlcli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) streamerAddCommand() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "Track a streamer and record them whenever they go live",
		ArgsUsage: streamerArgsUsage,
		Flags:     []cli.Flag{variantFlag()},
		Arguments: streamerArgs(),
		Action:    a.addStreamer,
	}
}

func (a *App) addStreamer(ctx context.Context, cmd *cli.Command) error {
	id, err := streamerRefFromArgs(cmd, "streamer add")
	if err != nil {
		return err
	}
	added, err := a.sendAdd(id, cmd.String("variant"))
	if err != nil {
		return err
	}
	// Report the identity the add resolved to. For a URL it is the only way to
	// learn what the URL named, and it is what every later command needs.
	fmt.Printf("Added %s\n", added)
	return nil
}

// sendAdd carries the variant in the add itself. A separate set_variant call
// would resolve the same URL a second time, and would land after the add has
// already started the recording.
func (a *App) sendAdd(id streamer.StreamerRef, variant string) (streamer.StreamerRef, error) {
	return a.sendAddRequest("streamer.add", id, variant)
}

// sendRecordOnce is sendAdd for the one-shot endpoint, which takes the same
// payload and answers the same way.
func (a *App) sendRecordOnce(id streamer.StreamerRef, variant string) (streamer.StreamerRef, error) {
	return a.sendAddRequest("streamer.record_once", id, variant)
}

func (a *App) sendAddRequest(
	subject string,
	id streamer.StreamerRef,
	variant string,
) (streamer.StreamerRef, error) {
	res, err := vlapi.Request[vlapi.AddRequest, vlapi.AddResponse](
		a.nc,
		subject,
		vlapi.AddRequest{StreamerRef: id, Variant: variant},
	)
	if err != nil {
		return streamer.StreamerRef{}, err
	}
	return res.StreamerRef, nil
}

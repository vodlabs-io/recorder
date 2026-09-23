package vlcli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) streamerVariantCommand() *cli.Command {
	return &cli.Command{
		Name:      "variant",
		Usage:     "Set (or clear, when omitted) the variant override for a streamer",
		ArgsUsage: "<url> [variant] | <platform> <username> [variant]",
		Arguments: []cli.Argument{
			&cli.StringArg{Name: "platform"},
			&cli.StringArg{Name: "username"},
			&cli.StringArg{Name: "variant"},
		},
		Action: a.setStreamerVariant,
	}
}

// setStreamerVariant reads either form. The two are told apart by shape rather
// than by position: a platform name never looks like a URL, so a leading URL
// means the next argument is the variant.
func (a *App) setStreamerVariant(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "streamer variant"); err != nil {
		return err
	}
	first := strings.TrimSpace(cmd.StringArg("platform"))
	second := strings.TrimSpace(cmd.StringArg("username"))
	third := strings.TrimSpace(cmd.StringArg("variant"))

	var id streamer.StreamerRef
	var variant string
	var err error
	if looksLikeURL(first) {
		if third != "" {
			return fmt.Errorf("a URL names the streamer on its own; drop the extra %q\n%s",
				third, usageLine("streamer variant", "[variant]"))
		}
		id, err = streamerRef(first, "", "streamer variant", "[variant]")
		variant = second
	} else {
		id, err = streamerRef(first, second, "streamer variant", "[variant]")
		variant = third
	}
	if err != nil {
		return err
	}
	return a.sendSetVariant(id, variant)
}

func (a *App) sendSetVariant(id streamer.StreamerRef, variant string) error {
	res, err := vlapi.Request[vlapi.SetVariantRequest, string](
		a.nc,
		"streamer.set_variant",
		vlapi.SetVariantRequest{StreamerRef: id, Variant: variant},
	)
	if err != nil {
		return err
	}
	if res != nil && *res != "" {
		fmt.Printf("%s: %s\n", describeRef(id), *res)
	}
	return nil
}

// variantFlag returns the --variant flag used by add/record commands.
func variantFlag() cli.Flag {
	return &cli.StringFlag{
		Name: "variant",
		// No "v" alias: it already means --version at the root and --verbose on
		// flv-fix, and a value-taking -v matches neither convention.
		Usage: "Variant override for this streamer (e.g. best, worst, 720p)",
	}
}

package vlcli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

// streamerListItem mirrors the streamer.list response. Kept local to the CLI
// for the same reason accountStatus is: the daemon owns the shape, and the CLI
// only needs the fields it prints.
type streamerListItem struct {
	Platform        string `json:"platform"`
	Username        string `json:"username"`
	URL             string `json:"url,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	Status          string `json:"status,omitempty"`
	OneShot         bool   `json:"one_shot,omitempty"`
	Monitor         bool   `json:"monitor"`
	Record          bool   `json:"record"`
	Variant         string `json:"variant"`
	OutputTemplate  string `json:"output_template"`
	DeletePartFiles *bool  `json:"delete_part_files"`
	SplitSize       string `json:"split_size"`
	SplitDuration   string `json:"split_duration"`
	ArchiveDir      string `json:"archive_dir"`
}

func (a *App) streamerListCommand() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "List tracked streamers with their status and settings",
		Flags: []cli.Flag{
			jsonFlag(),
			&cli.StringFlag{
				Name:    "platform",
				Aliases: []string{"p"},
				Usage:   "Only show streamers on this platform",
			},
		},
		Action: a.runStreamerList,
	}
}

func (a *App) runStreamerList(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "streamer list"); err != nil {
		return err
	}
	res, err := vlapi.Request[any, []streamerListItem](a.nc, "streamer.list", nil)
	if err != nil {
		return err
	}
	items := *res

	if p := strings.ToLower(cmd.String("platform")); p != "" {
		kept := items[:0]
		for _, it := range items {
			if it.Platform == p {
				kept = append(kept, it)
			}
		}
		items = kept
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Platform != items[j].Platform {
			return items[i].Platform < items[j].Platform
		}
		return items[i].Username < items[j].Username
	})

	if cmd.Bool("json") {
		return emitJSON(items)
	}

	if len(items) == 0 {
		fmt.Printf("No streamers tracked. Add one with '%s record <url>'.\n", RootCommandName)
		return nil
	}

	w := newTable()
	fmt.Fprintln(w, "PLATFORM\tUSERNAME\tSTATUS\tRECORD\tVARIANT\tMODE")
	for _, it := range items {
		name := it.Username
		if it.DisplayName != "" && it.DisplayName != it.Username {
			name = fmt.Sprintf("%s (%s)", it.Username, it.DisplayName)
		}
		if it.Platform == "" && it.URL != "" {
			// A URL-only entry never resolved, so it has no identity to show.
			name = it.URL
		}
		mode := "monitor"
		switch {
		case it.OneShot:
			mode = "once"
		case !it.Monitor:
			mode = "paused"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(it.Platform), name, dash(it.Status), yesNo(it.Record), dash(it.Variant), mode)
	}
	return w.Flush()
}

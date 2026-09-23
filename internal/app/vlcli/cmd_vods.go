package vlcli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

type vodListRequest struct {
	Offset int  `json:"offset"`
	Limit  int  `json:"limit"`
	Rescan bool `json:"rescan"`
}

type vodListPage struct {
	Sessions         []vodindex.VOD `json:"sessions"`
	Total            int            `json:"total"`
	Offset           int            `json:"offset"`
	UnavailableRoots []string       `json:"unavailable_roots"`
	Scanning         bool           `json:"scanning"`
}

// vodsCommand lists the VOD Archive, which is also how to find the folder
// `vod merge` takes.
func (a *App) vodsCommand() *cli.Command {
	return &cli.Command{
		Name:    "vods",
		Aliases: []string{"recordings"},
		Usage:   "Browse recorded sessions",
		Before:  a.connectDaemon,
		Commands: []*cli.Command{
			{
				Name:    "list",
				Aliases: []string{"ls"},
				Usage:   "List recorded sessions, newest first",
				Flags: []cli.Flag{
					jsonFlag(),
					&cli.StringFlag{Name: "platform", Aliases: []string{"p"},
						Usage: "Only show sessions for this platform"},
					&cli.StringFlag{Name: "username", Aliases: []string{"u"},
						Usage: "Only show sessions for this streamer"},
					&cli.IntFlag{Name: "limit", Aliases: []string{"n"}, Value: 50,
						Usage: "Maximum sessions to show"},
					&cli.BoolFlag{Name: "paths",
						Usage: "Print only session folder paths, one per line (for piping into vod merge)"},
					&cli.BoolFlag{Name: "rescan",
						Usage: "Re-read the download and archive folders before listing"},
				},
				Action: a.runVodsList,
			},
		},
	}
}

func (a *App) runVodsList(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "vods list"); err != nil {
		return err
	}
	limit := int(cmd.Int("limit"))
	if limit <= 0 {
		limit = 50
	}
	res, err := vlapi.Request[vodListRequest, vodListPage](a.nc, "vods.list",
		vodListRequest{Limit: vodListFetchLimit, Rescan: cmd.Bool("rescan")})
	if err != nil {
		return err
	}

	sessions := filterVODs(res.Sessions, cmd.String("platform"), cmd.String("username"))
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}

	if cmd.Bool("paths") {
		for _, v := range sessions {
			fmt.Println(v.Dir)
		}
		return nil
	}
	if cmd.Bool("json") {
		return emitJSON(sessions)
	}

	for _, root := range res.UnavailableRoots {
		fmt.Printf("Warning: %s is not reachable; sessions there are not listed.\n", root)
	}
	if res.Scanning {
		fmt.Println("(still scanning — the list may grow)")
	}
	if len(sessions) == 0 {
		fmt.Println("No recordings found.")
		return nil
	}

	w := newTable()
	fmt.Fprintln(w, "STARTED\tPLATFORM\tUSERNAME\tDURATION\tSIZE\tSTATE\tFOLDER")
	for _, v := range sessions {
		started := "-"
		if !v.Start.IsZero() {
			started = v.Start.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			started, dash(v.Platform), dash(v.Username),
			shortDuration(v.DurationSec), humanBytes(uint64(max(v.TotalBytes, 0))),
			dash(v.State), filepath.Base(v.Dir))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(sessions) < res.Total {
		fmt.Printf("\nShowing %d of %d sessions. Use --limit to see more, --paths for full folder paths.\n",
			len(sessions), res.Total)
	}
	return nil
}

// vodListFetchLimit asks for the daemon's whole page so client-side platform
// and username filters see every session, not just the first screenful.
const vodListFetchLimit = 1000

func filterVODs(in []vodindex.VOD, platform, username string) []vodindex.VOD {
	if platform == "" && username == "" {
		return in
	}
	out := make([]vodindex.VOD, 0, len(in))
	for _, v := range in {
		if platform != "" && !strings.EqualFold(v.Platform, platform) {
			continue
		}
		if username != "" && !strings.EqualFold(v.Username, username) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// shortDuration renders seconds as h/m/s without the sub-second noise
// time.Duration.String() carries.
func shortDuration(sec float64) string {
	if sec <= 0 {
		return "-"
	}
	d := (time.Duration(sec) * time.Second).Round(time.Second)
	return d.String()
}

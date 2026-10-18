package vlcli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func (a *App) infoCommand() *cli.Command {
	return &cli.Command{
		Name:   "info",
		Usage:  "Print daemon configuration and file paths",
		Before: a.connectDaemon,
		Flags:  []cli.Flag{jsonFlag()},
		Action: a.runInfo,
	}
}

func (a *App) runInfo(ctx context.Context, cmd *cli.Command) error {
	info, err := vlapi.Request[any, vlapi.DaemonInfo](a.nc, "info", nil)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return emitJSON(struct {
			CLIVersion string `json:"cli_version"`
			*vlapi.DaemonInfo
		}{version.Version, info})
	}
	fmt.Printf("CLI Version:      %s\n", version.Version)
	fmt.Printf("Daemon Version:   %s\n", info.Version)
	fmt.Printf("Daemon Container: %s\n", strconv.FormatBool(info.Container))
	fmt.Printf("Daemon URL:       %s\n", info.NatsURL)
	fmt.Printf("Daemon Path:      %s\n", info.ExecutablePath)
	fmt.Printf("Config:           %s\n", info.ConfigFilePath)
	fmt.Printf("Streamers:        %s\n", info.StreamersFilePath)
	fmt.Printf("Downloads:        %s\n", info.DownloadDirectory)
	fmt.Printf("Log (daemon):     %s\n", info.DaemonLogFilePath)
	fmt.Printf("Log (session):    %s\n", info.SessionLogFilePath)
	fmt.Printf("Log (processing): %s\n", info.ProcessingLogFilePath)
	fmt.Printf("Log (cloud):      %s\n", info.CloudLogFilePath)
	fmt.Printf("Log (scraper):    %s\n", info.ScraperLogFilePath)
	printStorageRoots(info.StorageRoots)
	printSharedChecks()
	return nil
}

// printSharedChecks states what the free service asks of this machine. It is
// printed rather than left to the docs because it is the one thing `vod info`
// can say that the user has not chosen: their address is used to check
// streamers other people follow.
func printSharedChecks() {
	fmt.Println()
	fmt.Println("Shared checks:    on (free plan)")
	fmt.Println("  This recorder also checks streamers other people follow, from")
	fmt.Println("  this machine's IP address. Your platform logins are never used")
	fmt.Println("  for those checks and never leave this device.")
	fmt.Println("  See https://vodlabs.io/docs/data")
}

// printStorageRoots reports free space per Storage root — the distinct
// filesystems behind the download and archive folders, which is where a full
// disk actually shows up. Folders sharing a disk are listed under one root.
func printStorageRoots(roots []diskspace.Root) {
	if len(roots) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("Storage:")
	for _, r := range roots {
		label := ""
		if len(r.Paths) > 0 {
			label = r.Paths[0]
		}
		role := "archive"
		if r.Download {
			role = "download"
		}
		if r.State == diskspace.StateUnknown {
			// Not the same as "no space free": say so plainly rather than
			// printing a zero the user would read as a full disk.
			fmt.Printf("  %-8s %s\n           unavailable (%s)\n", role, label, r.Error)
			continue
		}
		fmt.Printf("  %-8s %s\n           %s free of %s (%s)\n",
			role, label,
			humanBytes(r.Usage.FreeBytes), humanBytes(r.Usage.TotalBytes), r.State)
		for _, extra := range r.Paths[1:] {
			fmt.Printf("           also: %s\n", extra)
		}
	}
}

// humanBytes formats a byte count for a person reading a terminal.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

package vlcli

import (
	"context"
	"log"
	"os"

	"github.com/Masterminds/semver/v3"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlupdate"
)

func (a *App) updateCommand() *cli.Command {
	return &cli.Command{
		Name:   "update",
		Usage:  "Update the daemon and CLI to the latest version",
		Before: a.connectDaemon,
		Action: a.runUpdates,
	}
}

func (a *App) runUpdates(ctx context.Context, cmd *cli.Command) error {
	res, err := a.updateDaemon()
	if err != nil {
		return err
	}
	log.Println(res.Message)

	// Docker: if res.Container is true, the image is updated via `docker compose pull/up`, so there's
	// nothing to restart on the host.
	if !res.Container && res.Updated {
		if err := a.restartDaemon(ctx, cmd.String("config"), res.DaemonVersion, false); err != nil {
			return err
		}
	}

	return a.updateCLI(ctx)
}

func (a *App) updateDaemon() (vlapi.UpdateResponse, error) {
	req := vlapi.UpdateRequest{
		RepoURL: a.cfg.BinaryRepo,
	}
	res, err := vlapi.Request[vlapi.UpdateRequest, vlapi.UpdateResponse](a.nc, "update", req)
	if err != nil {
		return vlapi.UpdateResponse{}, err
	}
	return *res, nil
}

func (a *App) updateCLI(ctx context.Context) error {
	repo := a.cfg.BinaryRepo
	currentVersion, err := semver.NewVersion(version.Version)
	if err != nil {
		return err
	}
	latestStr, err := vlupdate.LatestVersion(ctx, repo)
	if err != nil {
		return err
	}
	latestVersion, err := semver.NewVersion(latestStr)
	if err != nil {
		return err
	}
	if currentVersion.GreaterThanEqual(latestVersion) {
		log.Printf("CLI is up to date (%s)", currentVersion)
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "vod-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	newBin, err := vlupdate.Download(ctx, repo, latestVersion.String(), tmpDir, vlupdate.CLIArtifact())
	if err != nil {
		return err
	}
	if err := vlupdate.Replace(exePath, newBin); err != nil {
		return err
	}
	log.Printf("CLI updated to %s", latestVersion)
	return nil
}

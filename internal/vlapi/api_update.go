package vlapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Masterminds/semver/v3"
	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlupdate"
)

type UpdateRequest struct {
	RepoURL string `json:"repo_url"`
}

type UpdateResponse struct {
	DaemonVersion string `json:"daemon_version"`
	Message       string `json:"message"`
	Updated       bool   `json:"updated"`
	Container     bool   `json:"container"`
}

func (s *Service) handleUpdate(req micro.Request) {
	var updateReq UpdateRequest
	if err := json.Unmarshal(req.Data(), &updateReq); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	// Clients (e.g. the desktop app) may omit the repo and let the daemon use its
	// own configured bin_repo, which is the same source it downloads from.
	if updateReq.RepoURL == "" {
		updateReq.RepoURL = s.appConfig.Daemon.BinaryRepo
	}
	if updateReq.RepoURL == "" {
		respond(req, fail(errInvalidArg, "missing repo URL"))
		return
	}

	ctx := context.Background()
	currentVersion, err := semver.NewVersion(version.Version)
	if err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	latestStr, err := vlupdate.LatestVersion(ctx, updateReq.RepoURL)
	if err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	latestVersion, err := semver.NewVersion(latestStr)
	if err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	inContainer := runningInContainer()
	if currentVersion.GreaterThanEqual(latestVersion) {
		respond(req, OK(UpdateResponse{
			DaemonVersion: currentVersion.String(),
			Message:       fmt.Sprintf("Daemon is up to date (%s)", currentVersion),
			Updated:       false,
			Container:     inContainer,
		}))
		return
	}

	// In a container the daemon binary is part of an immutable, ephemeral image,
	// so we can't swap it in place. Tell the user to pull the new image instead.
	if inContainer {
		respond(req, OK(UpdateResponse{
			DaemonVersion: latestVersion.String(),
			Message: fmt.Sprintf(
				"A new version (%s) is available. Update the container with:\n  docker compose pull && docker compose up -d",
				latestVersion),
			Updated:   false,
			Container: true,
		}))
		return
	}

	cacheDir, err := vlupdate.CacheDir(latestVersion.String())
	if err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	if _, err := vlupdate.Download(
		ctx,
		updateReq.RepoURL,
		latestVersion.String(),
		cacheDir,
		vlupdate.DaemonArtifact()); err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	respond(req, OK(UpdateResponse{
		DaemonVersion: latestVersion.String(),
		Message:       fmt.Sprintf("Downloaded version %s", latestVersion),
		Updated:       true,
	}))
	s.logger.Info("Downloaded latest daemon version", "old", currentVersion, "new", latestVersion)
}

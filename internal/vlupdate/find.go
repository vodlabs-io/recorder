package vlupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

const binaryName = "vlrecd"

func binName() string {
	if runtime.GOOS == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}

// artifact identifies a release binary published in the GitHub release: the
// archive's filename prefix (e.g. "daemon" → daemon_<platform>.tar.gz) and the
// binary file inside that archive (e.g. "vlrecd").
type artifact struct {
	Name       string
	binaryName string
}

func DaemonArtifact() artifact {
	return artifact{Name: "daemon", binaryName: binName()}
}

func CLIArtifact() artifact {
	name := "vod"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return artifact{Name: "cli", binaryName: name}
}

// CacheDir returns the per-version daemon cache directory:
// <UserCacheDir>/vodlabs/recorder/daemon/<version>/<os>_<arch>/
func CacheDir(version string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user cache directory: %w", err)
	}
	platform := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	return filepath.Join(base, "vodlabs", "recorder", "daemon", version, platform), nil
}

// Locate searches for a vlrecd binary matching the expected version. Lookup order:
//  1. cfg.Daemon.ExePath (errors if set but invalid — explicit override)
//  2. Same directory as the running CLI executable
//  3. exec.LookPath("vlrecd")
//  4. The versioned cache directory
//
// Returns the path to a binary whose --version output matches expectedVersion.
// Returns ErrNotFound if no candidate matched.
func Locate(ctx context.Context, exePath string, expectedVersion string) (string, error) {
	if p := exePath; p != "" {
		if err := checkVersion(ctx, p, expectedVersion); err != nil {
			return "", fmt.Errorf("daemon.exe_path %q: %w", p, err)
		}
		return p, nil
	}

	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), binName()))
	}
	if p, err := exec.LookPath(binaryName); err == nil {
		candidates = append(candidates, p)
	}
	if dir, err := CacheDir(expectedVersion); err == nil {
		candidates = append(candidates, filepath.Join(dir, binName()))
	}

	for _, p := range candidates {
		if err := checkVersion(ctx, p, expectedVersion); err == nil {
			return p, nil
		}
	}
	return "", ErrNotFound
}

var ErrNotFound = errors.New("no compatible daemon binary found")

// checkVersion runs `path --version` with a short timeout and compares the trimmed
// stdout to want.
func checkVersion(ctx context.Context, path, want string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return fmt.Errorf("running --version: %w", err)
	}
	got := strings.TrimSpace(string(out))
	gotVer, err := semver.NewVersion(got)
	if err != nil {
		return fmt.Errorf("parsing %q --version output %q: %w", path, got, err)
	}
	wantVer, err := semver.NewVersion(want)
	if err != nil {
		return fmt.Errorf("parsing expected version %q: %w", want, err)
	}
	if !gotVer.Equal(wantVer) {
		return fmt.Errorf("version mismatch: got %q want %q", got, want)
	}
	return nil
}

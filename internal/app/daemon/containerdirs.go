package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// checkContainerDir fails startup in a container when the daemon can't write
// to dir, naming the uid/gid it runs as and the fix. A bind mount Docker had
// to create is owned by root, which the non-root image can't write to; failing
// here says so instead of failing later on whichever file is written first.
func checkContainerDir(dir string) error {
	if !vlconfig.RunningInContainer() || strings.TrimSpace(dir) == "" {
		return nil
	}
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(dir, ".vlrec-writable-*"); err == nil {
			f.Close()
			os.Remove(f.Name())
			return nil
		}
	}
	uid, gid := os.Getuid(), os.Getgid()
	return fmt.Errorf("%s is not writable by the container's user (uid %d, gid %d): %w\n"+
		"Give that user the folder mounted at %s, e.g. on the host:\n"+
		"  sudo chown -R %d:%d <host folder>\n"+
		"or run the container as the folder's owner with user: \"<uid>:<gid>\" "+
		"(PUID/PGID in the compose file's .env). See docs/docker.md",
		dir, uid, gid, err, dir, uid, gid)
}

// configDir and cacheDir are the folders the daemon keeps its state and its
// cache in.
func configDir() string { return filepath.Dir(vlconfig.DefaultFilePath("config.toml")) }
func cacheDir() string  { return filepath.Dir(vlconfig.DefaultCacheFilePath("platforms.json")) }

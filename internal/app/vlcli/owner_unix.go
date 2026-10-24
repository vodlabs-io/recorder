//go:build !windows

package vlcli

import (
	"fmt"
	"os"
	"syscall"
)

// refuseForeignOwner stops root from writing into a config folder another
// user owns (`docker compose exec -u root`), which would leave files the
// daemon can't read.
func refuseForeignOwner(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	return fmt.Errorf("running as root, but %s belongs to uid %d; run this as that user "+
		"(drop `-u root` from `docker compose exec`) so the daemon can still read what it writes", dir, st.Uid)
}

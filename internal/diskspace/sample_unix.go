//go:build !windows

package diskspace

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

type sysSampler struct{}

func (sysSampler) Sample(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("reading free space for %s: %w", path, err)
	}
	dev, err := deviceID(path)
	if err != nil {
		return Usage{}, err
	}
	bsize := blockSize(&st)
	return Usage{
		TotalBytes: st.Blocks * bsize,
		// Bavail, not Bfree: blocks reserved for root are not ours to spend, and
		// counting them would promise space a recording cannot actually use.
		FreeBytes: st.Bavail * bsize,
		Device:    dev,
	}, nil
}

// deviceID returns the id of the filesystem path lives on. Two paths with the
// same id are on one filesystem, which is what lets a download folder and an
// archive folder on the same disk collapse into a single Storage root.
func deviceID(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("identifying filesystem for %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("identifying filesystem for %s: unexpected stat type %T", path, fi.Sys())
	}
	return strconv.FormatUint(uint64(st.Dev), 10), nil
}

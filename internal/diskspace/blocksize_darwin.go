package diskspace

import "syscall"

// blockSize is the unit the statfs block counts are expressed in.
//
// macOS has no f_frsize: its statfs defines f_bsize as the fundamental block
// size that f_blocks and f_bavail are counted in, so it is the right multiplier
// here. (Linux follows the POSIX statvfs split instead — see the linux file.)
func blockSize(st *syscall.Statfs_t) uint64 {
	return uint64(st.Bsize)
}

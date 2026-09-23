package diskspace

import "syscall"

// blockSize is the unit the statfs block counts are expressed in.
//
// POSIX counts f_blocks/f_bfree/f_bavail in f_frsize, not f_bsize: f_bsize is
// the preferred I/O size, which a filesystem is free to report much larger. On
// ext4 the two are both 4 KiB and the difference never shows, but Docker
// Desktop's virtiofs mounts advertise a 1 MiB f_bsize against 4 KiB blocks —
// scaling by the wrong one there reports a 466 GiB disk as 116 TiB.
func blockSize(st *syscall.Statfs_t) uint64 {
	if st.Frsize > 0 {
		return uint64(st.Frsize)
	}
	return uint64(st.Bsize)
}

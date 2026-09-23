package diskspace

import (
	"syscall"
	"testing"
)

// TestBlockSizePrefersFrsize pins the multiplier statfs block counts are in.
//
// The numbers are a real Docker Desktop virtiofs mount over a 466 GiB disk: it
// advertises a 1 MiB f_bsize against 4 KiB blocks. Scaling by f_bsize reported
// that disk as 116 TiB of which 31 TiB free, which reads as "space is not a
// concern" on a machine that is nearly full — and the low-space warning and the
// recording floors are both computed from it.
func TestBlockSizePrefersFrsize(t *testing.T) {
	st := syscall.Statfs_t{
		Bsize:  1 << 20, // preferred I/O size, not the block unit
		Frsize: 4096,
		Blocks: 122061322,
		Bavail: 32944531,
	}
	if got := blockSize(&st); got != 4096 {
		t.Fatalf("blockSize = %d, want 4096 (f_frsize)", got)
	}

	const gib = 1 << 30
	total := st.Blocks * blockSize(&st)
	free := st.Bavail * blockSize(&st)
	if gotGiB := total / gib; gotGiB != 465 {
		t.Errorf("total = %d GiB, want 465", gotGiB)
	}
	if gotGiB := free / gib; gotGiB != 125 {
		t.Errorf("free = %d GiB, want 125", gotGiB)
	}
}

// Not every filesystem reports f_frsize; falling back to f_bsize is right where
// it is absent, and on the common case the two agree anyway.
func TestBlockSizeFallsBackToBsize(t *testing.T) {
	st := syscall.Statfs_t{Bsize: 4096, Frsize: 0}
	if got := blockSize(&st); got != 4096 {
		t.Fatalf("blockSize with no f_frsize = %d, want the 4096 f_bsize", got)
	}
}

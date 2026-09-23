package vodmove

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// copyBufSize is the chunk size for the verified copy path. Large enough to keep
// network transfers efficient without holding much memory.
const copyBufSize = 1 << 20 // 1 MiB

// localDir is a Store backed by a local or network directory rooted at Root.
// relPath is joined under Root to form the destination path.
type localDir struct {
	Root string
}

// Store places localPath at Root/relPath. It first attempts an atomic rename
// (fast when source and destination share a volume) and falls back to a
// verified copy (temp file → fsync → size check → rename) on any rename error,
// which includes the cross-device case (EXDEV) that motivates this feature.
func (d *localDir) Store(ctx context.Context, localPath, relPath string) error {
	destPath := filepath.Join(d.Root, filepath.FromSlash(relPath))

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create destination dir: %w", err)
	}

	srcInfo, srcErr := os.Stat(localPath)
	destInfo, destErr := os.Stat(destPath)

	// Source already gone: only acceptable if the destination already has it
	// (a previous attempt completed before a crash). Otherwise it's a real
	// error — there is nothing to move.
	if srcErr != nil {
		if os.IsNotExist(srcErr) && destErr == nil {
			return nil
		}
		if os.IsNotExist(srcErr) {
			// Typed rather than a wrapped string: a missing source and an
			// unreachable destination are the same OS error, and callers have to
			// tell them apart to report and schedule them correctly.
			return &ErrSourceMissing{Path: localPath, err: srcErr}
		}
		return fmt.Errorf("stat source: %w", srcErr)
	}

	// Destination already present: identical size means a prior attempt already
	// copied it, so we're done (idempotent re-move). A different size is a stale
	// partial we must overwrite.
	if destErr == nil {
		if destInfo.Size() == srcInfo.Size() {
			return nil
		}
	}

	// Fast path: atomic rename. Windows rename fails if the destination exists,
	// so remove a stale destination first (the size check above already decided
	// it is safe to overwrite).
	if runtime.GOOS == "windows" && destErr == nil {
		_ = os.Remove(destPath)
	}
	if err := os.Rename(localPath, destPath); err == nil {
		return nil
	}

	// Fallback: verified copy. Any rename failure (cross-device, permissions,
	// network FS quirks) lands here.
	return d.copyVerified(ctx, localPath, destPath, srcInfo.Size())
}

// copyVerified copies src to dst via a temp file, fsyncs it, verifies the byte
// count matches wantSize, then atomically renames it into place. On any failure
// only the temp file is removed; src is never touched.
func (d *localDir) copyVerified(ctx context.Context, src, dst string, wantSize int64) error {
	tmp := dst + ".part"
	// Remove a stale temp file left by a daemon killed mid-copy.
	_ = os.Remove(tmp)

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	_, copyErr := copyChunks(ctx, out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("copy to temp file: %w", copyErr)
	}

	info, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stat temp file: %w", err)
	}
	if info.Size() != wantSize {
		_ = os.Remove(tmp)
		return fmt.Errorf("size mismatch after copy: wrote %d, expected %d", info.Size(), wantSize)
	}

	// Windows cannot rename over an existing file; clear any stale destination.
	if runtime.GOOS == "windows" {
		_ = os.Remove(dst)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp file into place: %w", err)
	}

	syncDir(filepath.Dir(dst))
	return nil
}

// copyChunks copies from in to out in fixed-size chunks, honoring ctx
// cancellation between chunks so a daemon shutdown aborts promptly. It returns
// the number of bytes written.
func copyChunks(ctx context.Context, out io.Writer, in io.Reader) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			w, werr := out.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w < n {
				return total, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// syncDir best-effort fsyncs a directory so the rename is durable. Directory
// fsync is a no-op/unsupported on Windows, so it is skipped there.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

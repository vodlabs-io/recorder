package processing

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/vodmove"
)

// preflightMove refuses a move the destination cannot hold, before a single
// byte is written, so a doomed attempt costs one statfs rather than filling the
// disk, and the UI can show how much to free. A destination that cannot be
// sized or probed is not failed here, so the move's real error surfaces.
func (s *Service) preflightMove(ctx context.Context, mj *moveJob) error {
	size, err := s.moveSizeBytes(mj)
	if err != nil {
		s.logger.Warn("Could not size move, attempting it anyway",
			"id", mj.id, "kind", mj.kind, "error", err)
		return nil
	}
	if size == 0 {
		return nil // nothing left to move (an interrupted sweep, already drained)
	}

	usage, err := diskspace.ProbeNearest(ctx, s.sampler, mj.dest, diskspace.DefaultProbeTimeout)
	if err != nil {
		return fmt.Errorf("checking free space at %s: %w", mj.dest, err)
	}
	if !diskspace.Fits(usage, size) {
		return &vodmove.ErrNoSpace{
			Dest:       mj.dest,
			NeedBytes:  size,
			FreeBytes:  usage.FreeBytes,
			ShortBytes: diskspace.Shortfall(usage, size),
		}
	}
	return nil
}

// moveSizeBytes reports how many bytes the job still has to write to the
// destination. A sweep is sized by what remains in the local folder, so a sweep
// resuming after a partial failure asks for only what it has left to do.
func (s *Service) moveSizeBytes(mj *moveJob) (uint64, error) {
	switch mj.kind {
	case "file":
		info, err := os.Stat(filepath.Join(mj.baseOutDir, mj.fileName))
		if os.IsNotExist(err) {
			// Nothing to size, and not an error to report here: the destination
			// may already hold the file, which only the move itself can check.
			// Warning here too would log one cause twice.
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		return uint64(info.Size()), nil

	case "sweep":
		var total uint64
		err := filepath.WalkDir(mj.baseOutDir, func(p string, e fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case e.IsDir():
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			total += uint64(info.Size())
			return nil
		})
		if os.IsNotExist(err) {
			return 0, nil // already swept
		}
		if err != nil {
			return 0, err
		}
		return total, nil

	default:
		return 0, fmt.Errorf("unknown move kind %q", mj.kind)
	}
}

// SetSampler replaces the free-space sampler. For tests: a full disk is not
// something CI can reproduce.
func (s *Service) SetSampler(sampler diskspace.Sampler) { s.sampler = sampler }

// SignalSpaceAvailable wakes the processing worker after space has been freed,
// so jobs deferred by hasScratchSpace resume without waiting for the next
// recording to finish. Non-blocking and safe to call repeatedly.
func (s *Service) SignalSpaceAvailable() {
	select {
	case s.spaceAvailable <- struct{}{}:
	default:
	}
}

// hasScratchSpace reports whether there is room on the download root to run a
// post-processing job.
//
// Processing writes its output alongside the input, so without room jobs are
// deferred, never dropped. A probe that fails is not treated as "no room".
func (s *Service) hasScratchSpace(ctx context.Context, downloadDir string) bool {
	if downloadDir == "" {
		return true
	}
	usage, err := diskspace.ProbeNearest(ctx, s.sampler, downloadDir, diskspace.DefaultProbeTimeout)
	if err != nil {
		s.logger.Warn("Could not check free space for processing; continuing",
			"dir", downloadDir, "error", err)
		return true
	}
	return diskspace.CanStart(usage)
}

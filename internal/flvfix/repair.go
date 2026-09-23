package flvfix

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// RepairFile sanitizes one raw FLV capture into clean, remux-ready files, one
// per distinct codec configuration (a configuration that flips away and back
// keeps appending to its file). Outputs are named "<stem>.ts", "<stem>_2.ts",
// … in order of first appearance. Returns the created paths and the final
// sanitizer stats.
func RepairFile(inPath, stem string, logger *slog.Logger) ([]string, Stats, error) {
	src, err := os.Open(inPath)
	if err != nil {
		return nil, Stats{}, err
	}
	defer src.Close()

	var (
		outputs []string
		handles []*os.File
		byKey   = map[string]*os.File{}
	)
	defer func() {
		for _, f := range handles {
			f.Close()
		}
	}()
	newFile := func() (*os.File, error) {
		name := stem + ".ts"
		if n := len(outputs); n > 0 {
			name = fmt.Sprintf("%s_%d.ts", stem, n+1)
		}
		f, err := os.Create(name)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, name)
		handles = append(handles, f)
		return f, nil
	}

	cur, err := newFile()
	if err != nil {
		return nil, Stats{}, err
	}
	s := New(cur, logger, Options{})
	firstKeyed := false

	rotate := func() error {
		if !firstKeyed {
			// The first output was created before any config was known;
			// register it under the initial config's key.
			byKey[s.ConfigKey()] = cur
			firstKeyed = true
		}
		next := byKey[s.pendingConfigKey()]
		if next == nil {
			var err error
			if next, err = newFile(); err != nil {
				return err
			}
			byKey[s.pendingConfigKey()] = next
		}
		cur = next
		return s.RotateOutput(next)
	}

	buf := make([]byte, 1<<20)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			_, werr := s.Write(buf[:n])
			for errors.Is(werr, ErrConfigChange) {
				werr = rotate()
			}
			if werr != nil {
				return outputs, s.Stats(), werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return outputs, s.Stats(), rerr
		}
	}
	ferr := s.Finish()
	for errors.Is(ferr, ErrConfigChange) {
		if ferr = rotate(); ferr == nil {
			ferr = s.Finish()
		}
	}
	for _, f := range handles {
		if cerr := f.Sync(); cerr != nil && ferr == nil {
			ferr = cerr
		}
	}
	return outputs, s.Stats(), ferr
}

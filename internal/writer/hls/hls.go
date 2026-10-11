// Package hls records a Broadcast to a directory of segments and a VOD
// playlist, for processing to assemble once the Session ends.
//
// Reading the Origin is hlsingest's job; this is the Sink it hands bytes to.
package hls

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/vodlabs-io/recorder/internal/provenance"
	"github.com/vodlabs-io/recorder/internal/writer/hlsingest"
)

const (
	fmp4InitExt = ".mp4"
	fmp4SegExt  = ".m4s"
)

func playlistVersion(isFMP4 bool) int {
	if isFMP4 {
		return 6
	}
	return 3
}

// Config holds the configuration for HLS recording
type Config struct {
	URL          string
	OutDir       string
	PollInterval time.Duration
	UserAgent    string
	Headers      map[string]string // HTTP headers in "Key: Value" format
	Retries      int
	FileName     string
	// Variant is the session's quality preference ("best", "worst", "720p", …),
	// used only when URL turns out to be a master playlist. Empty means best.
	Variant string
	// URLTTL and RefreshURL cover an Origin that signs a playlist for less than
	// a Broadcast lasts; see hlsingest.Config.
	URLTTL     time.Duration
	RefreshURL func(context.Context) (string, error)
	// MouflonKeys decrypts encrypted segment names; see hlsingest.Config.
	MouflonKeys map[string]string
	// Provenance is written as a playlist comment; see provenance.Record.
	Provenance string
}

func Record(
	ctx context.Context,
	cfg Config,
	client *http.Client,
	logger *slog.Logger,
	progressCh chan<- int64,
) error {
	// Create index.m3u8 file and writer in the output dir
	idxFile, err := os.Create(filepath.Join(cfg.OutDir, cfg.FileName))
	if err != nil {
		return fmt.Errorf("create index.m3u8 file: %w", err)
	}
	defer logger.Debug("Closed index.m3u8 file")
	defer idxFile.Close()

	sink := &archiveSink{
		outDir:     cfg.OutDir,
		idxFile:    idxFile,
		idxWriter:  bufio.NewWriter(idxFile),
		logger:     logger,
		progressCh: progressCh,
		provenance: cfg.Provenance,
	}

	return hlsingest.Run(ctx, hlsingest.Config{
		URL:          cfg.URL,
		Variant:      cfg.Variant,
		UserAgent:    cfg.UserAgent,
		Headers:      cfg.Headers,
		Retries:      cfg.Retries,
		PollInterval: cfg.PollInterval,
		// Some Origins stamp a local time with no offset, which the parser
		// rejects outright, taking the whole playlist with it.
		SanitizeTimestamps: true,
		URLTTL:             cfg.URLTTL,
		RefreshURL:         cfg.RefreshURL,
		MouflonKeys:        cfg.MouflonKeys,
	}, client, logger, sink)
}

// archiveSink writes each segment to disk under a name of its own and keeps a
// VOD playlist alongside them, rewritten as each segment lands so an
// interrupted Session still leaves a readable Part behind.
type archiveSink struct {
	outDir     string
	idxFile    *os.File
	idxWriter  *bufio.Writer
	logger     *slog.Logger
	progressCh chan<- int64

	provenance     string
	segLines       []string
	targetDuration float64
	initCount      int
	isFMP4         bool
	// lastWrittenKeyLine dedups the EXT-X-KEY passthrough, which is emitted
	// only for methods this writer cannot decrypt itself.
	lastWrittenKeyLine string
}

// Init writes an fMP4 initialisation segment and names it on the playlist,
// ahead of the fragments it initialises.
func (s *archiveSink) Init(m hlsingest.Map, body []byte) error {
	s.isFMP4 = true
	name := fmt.Sprintf("init_%02d%s", s.initCount, fmp4InitExt)
	s.initCount++
	if err := os.WriteFile(filepath.Join(s.outDir, name), body, 0o644); err != nil {
		return fmt.Errorf("failed to write map file %s: %w", name, err)
	}
	s.segLines = append(s.segLines, fmt.Sprintf("#EXT-X-MAP:URI=%q", name))
	s.report(len(body))
	return nil
}

// Segment decrypts what it can, writes the segment, and appends it to the
// playlist.
func (s *archiveSink) Segment(seg hlsingest.Segment, body []byte) error {
	if seg.IsFMP4 {
		s.isFMP4 = true
	}
	data, decrypted := decryptSegment(seg, body, s.logger)
	s.recordKeyLine(seg.Key, decrypted)

	// A fragmented-MP4 segment is named for what it is rather than for whatever
	// the Origin called it: ffmpeg's HLS demuxer refuses a segment whose
	// extension does not match the container it detects, so CHZZK's ".m4v"
	// fragments would make the recorded playlist unreadable at assembly time.
	ext := fmp4SegExt
	if !s.isFMP4 {
		ext = ""
		if u, err := url.Parse(seg.URI); err == nil {
			ext = path.Ext(u.Path)
		}
		if ext == "" {
			ext = ".ts"
		}
	}
	name := fmt.Sprintf("%06d%s", seg.Sequence, ext)
	if err := os.WriteFile(filepath.Join(s.outDir, name), data, 0o644); err != nil {
		return fmt.Errorf("failed to write segment file %s: %w", name, err)
	}
	s.report(len(data))

	dur := seg.Duration
	if dur <= 0 {
		dur = s.targetDuration
	}
	if dur > s.targetDuration {
		s.targetDuration = dur
	}
	s.segLines = append(s.segLines, fmt.Sprintf("#EXTINF:%.3f,", dur), name)

	if err := s.writePlaylist(); err != nil {
		return fmt.Errorf("failed to write playlist: %w", err)
	}
	return nil
}

// Finish terminates the playlist however the ingest ended, including on a fetch
// or parse failure — the ordinary way a Broadcast ends. Without EXT-X-ENDLIST a
// demuxer reads the Part as live: it opens the last three segments and then
// blocks reloading, so assembly keeps seconds of a Session that ran for hours.
func (s *archiveSink) Finish(error) error {
	if err := writeFinalPlaylist(
		s.idxFile, s.idxWriter, s.segLines, s.targetDuration, s.isFMP4, s.provenance); err != nil {
		return fmt.Errorf("failed to write final playlist: %w", err)
	}
	s.logger.Debug("Wrote final playlist")
	return nil
}

func (s *archiveSink) report(n int) {
	select {
	case s.progressCh <- int64(n):
	default:
		// Channel full or closed, skip update
	}
}

// recordKeyLine carries an EXT-X-KEY through to the recorded playlist for the
// methods this writer cannot decrypt itself, so assembly can still be told the
// segments are encrypted.
func (s *archiveSink) recordKeyLine(key *hlsingest.Key, decrypted bool) {
	if key == nil {
		s.lastWrittenKeyLine = ""
		return
	}
	if decrypted {
		return
	}
	attrs := fmt.Sprintf("METHOD=%s", key.Method)
	if key.URI != "" {
		attrs += fmt.Sprintf(",URI=%q", key.URI)
	}
	if key.IVAttr != "" {
		attrs += fmt.Sprintf(",IV=%s", key.IVAttr)
	}
	line := "#EXT-X-KEY:" + attrs
	if line != s.lastWrittenKeyLine {
		s.segLines = append(s.segLines, line)
		s.lastWrittenKeyLine = line
	}
}

func (s *archiveSink) writePlaylist() error {
	if _, err := s.idxFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := s.idxFile.Truncate(0); err != nil {
		return err
	}
	s.idxWriter.Reset(s.idxFile)
	writePlaylistHeader(s.idxWriter, s.targetDuration, s.isFMP4, s.provenance)
	for _, line := range s.segLines {
		fmt.Fprintln(s.idxWriter, line)
	}
	return s.idxWriter.Flush()
}

// decryptSegment decrypts an AES-128 segment in place, reporting whether it
// did. Anything else is left as it arrived, for recordKeyLine to declare.
func decryptSegment(seg hlsingest.Segment, body []byte, logger *slog.Logger) ([]byte, bool) {
	key := seg.Key
	if key == nil || len(key.Body) != 16 {
		return body, false
	}
	if key.Method != "AES-128" && key.Method != "AES128" {
		return body, false
	}
	iv := key.IV
	if len(iv) == 0 {
		// With no IV attribute the media sequence number is the IV, per the
		// spec — here, the segment's position in what was handed over.
		iv = make([]byte, 16)
		n := seg.Sequence
		for i := 0; i < 8; i++ {
			iv[15-i] = byte(n & 0xFF)
			n >>= 8
		}
	}
	dec, err := decryptAES128CBC(body, key.Body, iv)
	if err != nil {
		logger.Error("Failed to decrypt segment", "error", err)
		return body, false
	}
	return dec, true
}

func writePlaylistHeader(w *bufio.Writer, target float64, isFMP4 bool, prov string) {
	td := int(target + 0.9999)
	if td <= 0 {
		td = 1
	}
	fmt.Fprintln(w, "#EXTM3U")
	if prov != "" {
		fmt.Fprintln(w, provenance.PlaylistComment(prov))
	}
	fmt.Fprintf(w, "#EXT-X-VERSION:%d\n", playlistVersion(isFMP4))
	fmt.Fprintf(w, "#EXT-X-TARGETDURATION:%d\n", td)
	fmt.Fprintln(w, "#EXT-X-MEDIA-SEQUENCE:0")
	fmt.Fprintln(w, "#EXT-X-PLAYLIST-TYPE:VOD")
}

func writeFinalPlaylist(
	f *os.File,
	w *bufio.Writer,
	segLines []string,
	target float64,
	isFMP4 bool,
	prov string,
) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	w.Reset(f)
	writePlaylistHeader(w, target, isFMP4, prov)
	for _, line := range segLines {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "#EXT-X-ENDLIST")
	return w.Flush()
}

func decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, errors.New("invalid key/iv size for AES-128-CBC")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		pad := aes.BlockSize - (len(ciphertext) % aes.BlockSize)
		ciphertext = append(ciphertext, make([]byte, pad)...)
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	out := make([]byte, len(ciphertext))
	mode.CryptBlocks(out, ciphertext)
	if n := len(out); n > 0 {
		pad := int(out[n-1])
		if pad > 0 && pad <= aes.BlockSize && pad <= n {
			good := true
			for i := 0; i < pad; i++ {
				if out[n-1-i] != byte(pad) {
					good = false
					break
				}
			}
			if good {
				out = out[:n-pad]
			}
		}
	}
	return out, nil
}

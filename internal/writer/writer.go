package writer

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/writer/flv"
	"github.com/vodlabs-io/recorder/internal/writer/hls"
)

// Config holds configuration for recording writers
type Config struct {
	StreamURL string
	Headers   map[string]string
	UserAgent string
	Retries   int
	// Sanitize enables in-flight FLV stream cleanup (flvwriter only).
	Sanitize bool
	// OnConfigKey receives the part's codec configuration key as the part
	// finishes (flvwriter only; see flv.Config.OnConfigKey).
	OnConfigKey func(key string)
	// Carryover holds the writer state that must survive this Part. A Config
	// is built per Part, so the Session owns it and passes the same pointer in
	// every time; nil disables the carry-over.
	Carryover *Carryover
	// Variant is the session's quality preference, needed only when a stream URL
	// turns out to be a master playlist (hlswriter only; see hls.Config.Variant).
	Variant string
	// StreamURLTTL is how long StreamURL stays usable, zero when it outlives the
	// Session, and RefreshStreamURL re-resolves it when it does not. Set together
	// (see hlsingest.Config.URLTTL and flv.URLRenewer).
	StreamURLTTL     time.Duration
	RefreshStreamURL func(context.Context) (string, error)
	// MouflonKeys decrypts the segment names of a playlist that publishes them
	// encrypted (hlswriter only; see hlsingest.Config.MouflonKeys).
	MouflonKeys map[string]string
	// Provenance describes the build and session that produced this Part, for
	// writers that author their own output files rather than passing the
	// origin's bytes through unchanged.
	Provenance string
}

// Carryover is writer state scoped to a Session rather than a Part: the FLV
// writer continues one stream across the Part boundary a codec-configuration
// change creates, instead of reconnecting and losing the sequence header that
// announced it.
//
// A Session creates one, hands the same pointer to every Part's Config, and
// Closes it as the Session ends. Parts run one at a time, so it needs no
// locking.
type Carryover struct {
	flvHandoff *flv.Handoff
}

// NewCarryover returns the per-Session writer state.
func NewCarryover() *Carryover {
	return &Carryover{flvHandoff: &flv.Handoff{}}
}

// Close releases anything still held open. Safe on a nil Carryover, so a
// Session that ended before building one can call it unconditionally.
func (c *Carryover) Close() {
	if c == nil {
		return
	}
	c.flvHandoff.Close()
}

// PartSpec is a Part's shape on disk: where a writer puts the files of one
// recording attempt, and what identifies them afterwards.
//
// It is the writer's answer, not the session's. A Part is a file for one writer
// and a directory for another, and only the writer knows which — so the session
// asks for one rather than deciding from the writer's name.
type PartSpec struct {
	// Dir is where this Part's files live.
	Dir string
	// FileName is the file the writer opens within Dir, empty when the writer
	// names its own files.
	FileName string
	// Path identifies the Part for cleanup and for the manifest — the file for
	// a writer that produces one, the directory for a writer that produces many.
	Path string
	// Retries is this writer's own budget for one Part. The session may lower
	// it, so it is a default rather than a limit.
	Retries int
}

// Writer defines the interface for recording writers (hls, flv, etc.)
type Writer interface {
	Name() string
	// PreparePart lays out one Part under baseOutDir, named for stem, and
	// creates whatever directory it needs. The returned spec is handed straight
	// back to Record.
	PreparePart(baseOutDir, stem string) (PartSpec, error)
	// DiscardPart removes a Part that recorded nothing. A writer whose output
	// is shared across the Parts of a Session has nothing to remove and does
	// nothing here.
	DiscardPart(part PartSpec) error
	Record(
		ctx context.Context,
		part PartSpec,
		cfg *Config,
		client *http.Client,
		logger *slog.Logger,
		progressCh chan<- int64,
	) error
}

// hlsWriter implements the Writer interface for HLS recording
type hlsWriter struct{}

func (h *hlsWriter) Name() string {
	return "hlswriter"
}

// PreparePart gives each Part a subdirectory of its own, so a reconnect cannot
// overwrite the segments of the Part before it.
func (h *hlsWriter) PreparePart(baseOutDir, stem string) (PartSpec, error) {
	dir := filepath.Join(baseOutDir, stem)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PartSpec{}, fmt.Errorf("create part directory: %w", err)
	}
	return PartSpec{Dir: dir, FileName: "index.m3u8", Path: dir, Retries: 3}, nil
}

func (h *hlsWriter) DiscardPart(part PartSpec) error {
	if err := os.RemoveAll(part.Path); err != nil {
		return fmt.Errorf("delete empty part directory: %w", err)
	}
	return nil
}

func (h *hlsWriter) Record(
	ctx context.Context,
	part PartSpec,
	cfg *Config,
	client *http.Client,
	logger *slog.Logger,
	progressCh chan<- int64,
) error {
	hlsConfig := hls.Config{
		URL:          cfg.StreamURL,
		OutDir:       part.Dir,
		UserAgent:    cfg.UserAgent,
		Headers:      cfg.Headers,
		PollInterval: 2 * time.Second,
		Retries:      cfg.Retries,
		FileName:     part.FileName,
		Variant:      cfg.Variant,
		URLTTL:       cfg.StreamURLTTL,
		RefreshURL:   cfg.RefreshStreamURL,
		MouflonKeys:  cfg.MouflonKeys,
		Provenance:   cfg.Provenance,
	}

	return hls.Record(ctx, hlsConfig, client, logger, progressCh)
}

// flvWriter implements the Writer interface for FLV recording
type flvWriter struct{}

func (f *flvWriter) Name() string {
	return "flvwriter"
}

// PreparePart writes each Part as one file beside the others in the session
// directory, which already exists by the time a Part starts.
func (f *flvWriter) PreparePart(baseOutDir, stem string) (PartSpec, error) {
	if err := os.MkdirAll(baseOutDir, 0o755); err != nil {
		return PartSpec{}, fmt.Errorf("create part directory: %w", err)
	}
	name := stem + ".ts"
	return PartSpec{
		Dir:      baseOutDir,
		FileName: name,
		Path:     filepath.Join(baseOutDir, name),
		Retries:  2,
	}, nil
}

func (f *flvWriter) DiscardPart(part PartSpec) error {
	if err := os.Remove(part.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete empty part file: %w", err)
	}
	return nil
}

func (f *flvWriter) Record(
	ctx context.Context,
	part PartSpec,
	cfg *Config,
	client *http.Client,
	logger *slog.Logger,
	progressCh chan<- int64,
) error {
	var handoff *flv.Handoff
	if cfg.Carryover != nil {
		handoff = cfg.Carryover.flvHandoff
	}
	flvConfig := flv.Config{
		URL:            cfg.StreamURL,
		OutDir:         part.Dir,
		UserAgent:      cfg.UserAgent,
		Headers:        cfg.Headers,
		Retries:        cfg.Retries,
		ChunkSize:      8192,
		ReconnectDelay: 2 * time.Second,
		FileName:       part.FileName,
		Sanitize:       cfg.Sanitize,
		OnConfigKey:    cfg.OnConfigKey,
		Handoff:        handoff,
		URLTTL:         cfg.StreamURLTTL,
		RefreshURL:     cfg.RefreshStreamURL,
	}

	return flv.Record(ctx, flvConfig, client, logger, progressCh)
}

var (
	writerMu sync.RWMutex
	writers  = make(map[string]Writer)
)

func init() {
	register(&hlsWriter{})
	register(&flvWriter{})
}

// register registers a new writer
func register(w Writer) {
	if w == nil {
		return
	}
	writerMu.Lock()
	defer writerMu.Unlock()
	writers[w.Name()] = w
}

// Get retrieves a writer by name
func Get(name string) (Writer, bool) {
	writerMu.RLock()
	defer writerMu.RUnlock()
	w, ok := writers[name]
	return w, ok
}

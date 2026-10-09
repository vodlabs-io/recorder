package flv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/agora"
	"github.com/vodlabs-io/recorder/internal/flvfix"
	"github.com/vodlabs-io/recorder/internal/trtc"
	"github.com/vodlabs-io/recorder/internal/vlerror"
)

const (
	defaultChunkSize = 8192 // Streamlink's default chunk size
)

// Config holds the configuration for the FLV writer
type Config struct {
	URL       string
	OutDir    string
	UserAgent string
	// Headers are extra HTTP headers sent when fetching the stream (e.g.
	// Referer/Origin, and — for login-restricted streams served from a
	// cookie-scoped domain — a Cookie header). Applied on top of User-Agent.
	Headers        map[string]string
	Retries        int
	ChunkSize      int
	ReconnectDelay time.Duration
	FileName       string
	// Sanitize enables in-flight FLV cleanup (dedup, timeline repair, garbage
	// rejection). When false the raw stream is dumped byte-for-byte.
	Sanitize bool
	// OnConfigKey, when set, receives the part's codec configuration key as
	// the part finishes. Parts with equal keys are stream-copy compatible;
	// the session worker groups them into one VOD file.
	OnConfigKey func(key string)
	// Handoff carries the stream and its sanitizer from one part to the next
	// across a codec-configuration change. One per session; nil disables the
	// carry-over and every configuration change costs a reconnect.
	Handoff *Handoff
	// URLTTL and RefreshURL cover an Origin that grants an FLV URL for less
	// than a Broadcast lasts; see URLRenewer.
	URLTTL     time.Duration
	RefreshURL func(context.Context) (string, error)
}

// Handoff holds the live stream a codec-configuration change interrupted, so
// the next part reads on from it instead of reconnecting.
//
// A configuration change is announced by a sequence-header tag that the
// sanitizer keeps buffered until RotateOutput consumes it. Dropping the
// sanitizer loses that tag, and reconnecting rejoins the Origin after the
// switch, so the new part would record content its configuration does not
// describe.
//
// One Handoff serves a whole session and is not safe for concurrent use; parts
// run one at a time.
type Handoff struct {
	stream    io.ReadCloser
	cancel    context.CancelFunc
	sanitizer *flvfix.Sanitizer
}

// take removes and returns whatever the previous part left behind.
func (h *Handoff) take() (io.ReadCloser, context.CancelFunc, *flvfix.Sanitizer) {
	if h == nil || h.sanitizer == nil {
		return nil, nil, nil
	}
	stream, cancel, sanitizer := h.stream, h.cancel, h.sanitizer
	h.stream, h.cancel, h.sanitizer = nil, nil, nil
	return stream, cancel, sanitizer
}

// Parked reports whether a stream is waiting for the next part to pick up.
func (h *Handoff) Parked() bool { return h != nil && h.sanitizer != nil }

// Close releases a stream no part came back for. Safe on a nil or empty
// Handoff, so a session can call it unconditionally as it ends.
func (h *Handoff) Close() {
	stream, cancel, _ := h.take()
	if cancel != nil {
		cancel()
	}
	if stream != nil {
		stream.Close()
	}
}

type flvWriter struct {
	filePath          string
	file              *os.File
	sink              io.Writer // sanitizer wrapping file, or file itself
	sanitizer         *flvfix.Sanitizer
	client            *http.Client
	totalBytes        int64
	lastReportedBytes int64 // Tracks bytes already reported via progress channel
	config            Config
	logger            *slog.Logger
	progressCh        chan<- int64

	// Stream ownership. streamCancel aborts the in-flight request; it outlives
	// the part so a rotation can pass it on. writtenBase and statsBase anchor
	// this part's share of a sanitizer that spans several.
	streamCancel context.CancelFunc
	writtenBase  int64
	statsBase    flvfix.Stats
	handedOff    bool
}

// Record creates a new FLV writer and starts recording the stream
func Record(
	ctx context.Context,
	cfg Config,
	client *http.Client,
	logger *slog.Logger,
	progressCh chan<- int64,
) error {
	w := &flvWriter{
		client:     client,
		config:     cfg,
		logger:     logger,
		progressCh: progressCh,
	}

	return w.Run(ctx)
}

func (w *flvWriter) ensureOutputFile(fileName string) error {
	w.filePath = filepath.Join(w.config.OutDir, fileName)

	file, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}

	w.file = file
	return nil
}

// finishPart reports the part's codec config key to the session worker and
// logs this part's share of the sanitizer counters.
//
// A part handed on to the next one is not finished: the sanitizer keeps its
// buffer (and the sequence header in it) for RotateOutput, and Finish would
// mark end-of-input on a stream that has not ended.
func (w *flvWriter) finishPart() {
	if w.sanitizer == nil {
		return
	}
	if !w.handedOff {
		if err := w.sanitizer.Finish(); err != nil && !errors.Is(err, flvfix.ErrConfigChange) {
			w.logger.Warn("Failed to flush sanitizer", "error", err)
		}
	}
	if w.config.OnConfigKey != nil {
		w.config.OnConfigKey(w.sanitizer.ConfigKey())
	}
	w.logger.Info("Sanitizer summary", w.sanitizer.Stats().Since(w.statsBase).Attrs()...)
}

func (w *flvWriter) closeOutputFile() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
		w.logger.Debug("Output file closed")
	}
}

// openStream issues the stream request (or joins the Agora channel or TRTC room
// an agora:// or trtc:// URL names) on a context of its own so the response can outlive the part that
// opened it (see Handoff). guardStream ties it back
// to the part for every other kind of ending — including the dial, which a
// cancelled part must still abort.
func (w *flvWriter) openStream(ctx context.Context, url string) (io.ReadCloser, error) {
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.streamCancel = cancel
	release := w.guardStream(ctx)
	defer release()
	fail := func(err error) (io.ReadCloser, error) {
		cancel()
		w.streamCancel = nil
		return nil, err
	}

	if agora.IsURL(url) {
		stream, err := agora.Open(streamCtx, url, w.client, w.logger)
		if err != nil {
			return fail(fmt.Errorf("failed to join channel: %w", err))
		}
		return stream, nil
	}
	if trtc.IsURL(url) {
		stream, err := trtc.Open(streamCtx, url, w.client, w.logger)
		if err != nil {
			return fail(fmt.Errorf("failed to join room: %w", err))
		}
		return stream, nil
	}

	req, err := http.NewRequestWithContext(streamCtx, "GET", url, nil)
	if err != nil {
		return fail(fmt.Errorf("failed to create request: %w", err))
	}

	if w.config.UserAgent != "" {
		req.Header.Set("User-Agent", w.config.UserAgent)
	}
	// Apply extra headers (Referer/Origin, and Cookie for authenticated streams).
	// PRIVACY: any Cookie here came from the local encrypted store via the session
	// worker's host-guarded downloadHeaders(); it is only ever sent to the stream
	// host and is never logged.
	for key, value := range w.config.Headers {
		req.Header.Set(key, value)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fail(fmt.Errorf("failed to make request: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fail(fmt.Errorf("server returned status %d: %s", resp.StatusCode, resp.Status))
	}

	return resp.Body, nil
}

// guardStream aborts the in-flight request when the part's context ends, so a
// stalled or split part still tears the socket down. The returned release
// stops the guard without aborting, which is what a rotation wants; it must be
// called before the worker cancels the part's context.
func (w *flvWriter) guardStream(ctx context.Context) (release func()) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			if w.streamCancel != nil {
				w.streamCancel()
			}
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// closeStream ends the current request for good.
func (w *flvWriter) closeStream(stream io.ReadCloser) {
	if w.streamCancel != nil {
		w.streamCancel()
		w.streamCancel = nil
	}
	stream.Close()
}

// handOff parks the live stream and the sanitizer for the next part. It
// reports whether the carry-over happened; without a Handoff the caller closes
// the stream and the next part reconnects.
func (w *flvWriter) handOff(stream io.ReadCloser) bool {
	h := w.config.Handoff
	if h == nil || w.sanitizer == nil {
		return false
	}
	h.stream, h.cancel, h.sanitizer = stream, w.streamCancel, w.sanitizer
	w.streamCancel = nil
	w.handedOff = true
	return true
}

// resume adopts the stream and sanitizer the previous part left behind and
// rotates the sanitizer's output onto this part's file, which replays the
// buffered sequence header into it. Returns nil, nil when there is nothing to
// resume.
func (w *flvWriter) resume() (io.ReadCloser, error) {
	stream, cancel, sanitizer := w.config.Handoff.take()
	if sanitizer == nil {
		return nil, nil
	}
	w.sanitizer = sanitizer
	w.sink = sanitizer
	w.streamCancel = cancel
	w.writtenBase = sanitizer.Written()
	w.statsBase = sanitizer.Stats()
	if err := sanitizer.RotateOutput(w.file); err != nil {
		// A second configuration change was already buffered: this part is
		// over before it began. Anything else is a sink failure.
		if errors.Is(err, flvfix.ErrConfigChange) && w.handOff(stream) {
			return nil, err
		}
		w.closeStream(stream)
		return nil, fmt.Errorf("%w: %v", vlerror.ErrWriteFailed, err)
	}
	w.logger.Debug("Resumed stream across configuration change")
	return stream, nil
}

func (w *flvWriter) streamToFile(ctx context.Context, stream io.ReadCloser) error {
	chunkSize := w.config.ChunkSize
	if chunkSize == 0 {
		chunkSize = defaultChunkSize
	}

	buffer := make([]byte, chunkSize)

	w.logger.Debug("Starting data stream", "chunk_size", chunkSize)

	// Throttle progress updates to avoid rapid speed fluctuations
	lastProgressUpdate := time.Now()
	progressUpdateInterval := 500 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n, err := stream.Read(buffer)
		if n > 0 {
			if _, writeErr := w.sink.Write(buffer[:n]); writeErr != nil {
				// A codec-configuration change ends the part cleanly; the
				// worker rotates to a fresh output. Sink (disk) failures are
				// fatal write errors.
				if errors.Is(writeErr, flvfix.ErrConfigChange) {
					return writeErr
				}
				return fmt.Errorf("%w: %v", vlerror.ErrWriteFailed, writeErr)
			}
			// Track committed on-disk bytes: the sanitizer drops duplicates and
			// garbage, so progress, MinPartBytes and split limits all reflect
			// the real output size, not the network read size.
			if w.sanitizer != nil {
				w.totalBytes = w.sanitizer.Written() - w.writtenBase
			} else {
				w.totalBytes += int64(n)
			}

			// Update progress tracker only periodically to smooth speed display
			now := time.Now()
			if now.Sub(lastProgressUpdate) >= progressUpdateInterval {
				bytesIncrement := w.totalBytes - w.lastReportedBytes
				select {
				case w.progressCh <- bytesIncrement:
					w.lastReportedBytes = w.totalBytes
					lastProgressUpdate = now
				case <-ctx.Done():
					return ctx.Err()
				default:
					// Non-blocking: drop this progress update rather than stalling recording
					// when the progress consumer is slow or the channel buffer is full.
				}
			}
		}

		if err != nil {
			// Send final progress update for any remaining bytes
			if w.totalBytes > w.lastReportedBytes {
				bytesIncrement := w.totalBytes - w.lastReportedBytes
				select {
				case w.progressCh <- bytesIncrement:
				case <-ctx.Done():
				default:
					// Channel full or closed, skip final update
				}
			}

			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("stream read error: %w", err)
		}
	}
}

func (w *flvWriter) Run(ctx context.Context) error {
	if err := w.ensureOutputFile(w.config.FileName); err != nil {
		return err
	}
	defer w.closeOutputFile()

	// One sanitizer per stream, not per part: it persists across reconnects
	// within a part so the Origin's replay-on-reconnect is deduplicated against
	// already-written content, and across a configuration change so the tag
	// announcing the new configuration reaches the part that needs it.
	// finishPart is deferred last-in so it flushes before the file is closed.
	//
	// Every part is written through the sanitizer so the recorded file is
	// structurally valid FLV on a monotonic timeline whatever the Origin sent.
	// Sanitize chooses whether the timeline is also repaired: gaps glued,
	// replays deduplicated, backward jumps rebased.
	var resumed io.ReadCloser
	var err error
	if resumed, err = w.resume(); err != nil {
		defer w.finishPart()
		return err
	}
	defer func() {
		if resumed != nil {
			w.closeStream(resumed)
		}
	}()
	if w.sanitizer == nil {
		w.sanitizer = flvfix.New(w.file, w.logger, flvfix.Options{
			NoResume:        true,
			RepairsDisabled: !w.config.Sanitize,
		})
	}
	w.sink = w.sanitizer
	defer w.finishPart()

	urls := NewURLRenewer(ctx, w.config.URL, w.config.URLTTL, w.config.RefreshURL, w.logger)
	defer urls.Close()
	if resumed != nil {
		urls.MarkUsed()
	}

	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		maxRetries := w.config.Retries

		stream := resumed
		resumed = nil
		if stream == nil {
			if consecutiveErrors > 0 {
				w.logger.Info("Reconnecting stream",
					"attempt", consecutiveErrors,
					"max_retries", maxRetries)
			} else {
				w.logger.Debug("Opening stream")
			}

			var err error
			stream, err = w.openStream(ctx, urls.Next(ctx))
			if err != nil {
				consecutiveErrors++
				w.logger.Warn("Failed to open stream",
					"error", err)

				if consecutiveErrors <= maxRetries {
					w.logger.Debug("Waiting for reconnect", "delay", w.config.ReconnectDelay)
					select {
					case <-time.After(w.config.ReconnectDelay):
						continue
					case <-ctx.Done():
						return nil
					}
				}
				return fmt.Errorf("%w: %w", vlerror.ErrConnectFailed, err)
			}
			if consecutiveErrors > 0 {
				w.logger.Info("Connected. Resuming...")
				// Successfully connected, reset error counter
				consecutiveErrors = 0
			}
		}

		release := w.guardStream(ctx)
		err := w.streamToFile(ctx, stream)
		release()
		if syncErr := w.file.Sync(); syncErr != nil {
			w.closeStream(stream)
			return fmt.Errorf("failed to sync data to disk: %w", syncErr)
		}

		// A codec-configuration change is the one ending that keeps the
		// stream: the part is complete, the next one reads on from here.
		if errors.Is(err, flvfix.ErrConfigChange) {
			if !w.handOff(stream) {
				w.closeStream(stream)
			}
			return err
		}
		w.closeStream(stream)

		if err != nil {
			if errors.Is(err, vlerror.ErrWriteFailed) {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			if isRecoverableError(err) {
				w.logger.Debug("Segment completed, continuing stream")
				continue
			}

			consecutiveErrors++
			w.logger.Warn("Stream error", "error", err, "consecutive_errors", consecutiveErrors)

			maxRetries := w.config.Retries

			if consecutiveErrors > maxRetries {
				return fmt.Errorf("stream failed after %d consecutive errors: %w", maxRetries, err)
			}

			w.logger.Debug("Waiting for reconnect", "delay", w.config.ReconnectDelay)
			select {
			case <-time.After(w.config.ReconnectDelay):
				continue
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func isRecoverableError(err error) bool {
	// Consider EOF and connection reset as recoverable for streaming
	return err == io.EOF ||
		err == io.ErrUnexpectedEOF ||
		isNetworkError(err)
}

func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// Check for common network error strings
	errStr := err.Error()
	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "timeout")
}

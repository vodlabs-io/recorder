package session

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/vodlabs-io/recorder/internal/cookiemint"
	"github.com/vodlabs-io/recorder/internal/provenance"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/useragent"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vodmeta"
	"github.com/vodlabs-io/recorder/internal/writer"
)

// initSession creates output dir, logger, and generates a unique session ID
func (w *worker) initSession(str streamer.StreamerState) error {
	platform := str.Platform
	username := str.Username
	w.strKey = str.StreamerKey
	w.output.Platform = platform
	w.output.Username = username

	sessionID, err := gonanoid.New(10)
	if err != nil {
		return fmt.Errorf("failed to generate session ID: %w", err)
	}
	w.SessionID = sessionID
	w.output.SessionID = sessionID

	// Build the session output directory from the template, relative to DownloadDir.
	if w.output.BaseOutDir == "" {
		rel := renderOutputDir(w.config.OutputTemplate, platform, username, sessionID, time.Now())
		w.output.BaseOutDir = filepath.Join(w.config.DownloadDir, rel)
	}
	outDir := w.output.BaseOutDir
	w.pubOutDir.Store(&outDir)

	if err := os.MkdirAll(w.output.BaseOutDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	logFilePath := filepath.Join(w.output.BaseOutDir, "session.log")
	file, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open session log file: %w", err)
	}
	w.sessionLogFile = file

	textHandler := slog.NewTextHandler(w.sessionLogFile, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	w.logger = slog.New(textHandler).With("service", "session")

	w.logger = w.logger.With("session_id", sessionID)

	w.metaLog = vodmeta.NewLog(w.output.BaseOutDir, platform, username, sessionID)

	if strings.Contains(w.output.BaseOutDir, "{") {
		w.logger.Warn("Output directory contains unrecognized template placeholders",
			"out_dir", w.output.BaseOutDir, "template", w.config.OutputTemplate)
	}

	// Parse split limits; invalid values are ignored rather than fatal.
	if w.config.SplitSize != "" {
		if sz, err := vlconfig.ParseSplitSize(w.config.SplitSize); err != nil {
			w.logger.Warn("Ignoring invalid split_size", "value", w.config.SplitSize, "error", err)
		} else {
			w.splitSize = sz
		}
	}
	if w.config.SplitDuration != "" {
		if d, err := vlconfig.ParseSplitDuration(w.config.SplitDuration); err != nil {
			w.logger.Warn("Ignoring invalid split_duration", "value", w.config.SplitDuration, "error", err)
		} else {
			w.splitDuration = d
		}
	}

	w.logger.Info("Initialized session",
		"out_dir", w.output.BaseOutDir,
		"variant", w.config.Variant,
		"split_size", w.config.SplitSize,
		"split_duration", w.config.SplitDuration)

	return nil
}

// initStream initializes/logs stream information and selects the writer
func (w *worker) initStream(str streamer.StreamerState) error {
	// Select the stream based on the variant preference
	selectedStream := str.Streams.Select(w.config.Variant)

	if w.selectedStream == nil && selectedStream == nil {
		return fmt.Errorf("stream not found")
	}
	if selectedStream == nil {
		w.logger.Warn("Streamer has no streams, using previously selected stream",
			"selected_stream", w.selectedStream.QualityID)
		return nil
	} else {
		w.selectedStream = selectedStream
	}
	w.streamsTTL = time.Duration(str.StreamsTTL) * time.Second

	w.selectedHTTPClient = w.flvHTTPClient
	local := vlconfig.IsLocal(str.Platform)
	if local {
		w.selectedHTTPClient = guardedFLVClient
	}

	writerName := "flvwriter"
	if selectedStream.IsVP8() {
		// Neither FLV nor MP4 carries VP8 through ffmpeg; WebM does.
		writerName = "webmwriter"
	}
	if selectedStream.IsHLS() {
		writerName = "hlswriter"
		w.selectedHTTPClient = w.basicHTTPClient
		if local {
			w.selectedHTTPClient = guardedHLSClient
		}
	}
	// A platform whose stream answers only to cookies that expire within
	// seconds has them minted afresh for every request that needs them.
	w.selectedHTTPClient = cookiemint.Client(w.selectedHTTPClient, str.CookieMint)
	w.output.WriterName = writerName

	writerInstance, ok := writer.Get(writerName)
	if !ok {
		return fmt.Errorf("writer not found: %s", writerName)
	}
	if w.writer == nil || w.writer.Name() != writerInstance.Name() {
		w.logger.Info("Selected writer", "writer", writerInstance.Name())
	}
	w.writer = writerInstance

	if w.recAttempts > 0 {
		w.logger.Debug("Live metadata", str.LiveMetadata.Attrs()...)
	} else {
		w.logger.Info("Live metadata", str.LiveMetadata.Attrs()...)
	}

	w.logStreamVariants(str.Streams)

	return nil
}

// initPart sets up the output directory and writer configuration for a new recording part
func (w *worker) initPart() error {
	partName := fmt.Sprintf("pt%d_%s", w.output.PartIndex, w.selectedStream.QualityID)
	w.curStem = partName // stem fed to the split group on completion

	// The writer lays out its own Part: a file for one writer, a directory for
	// another, and only it knows which.
	part, err := w.writer.PreparePart(w.output.BaseOutDir, partName)
	if err != nil {
		return err
	}
	w.curPart = part

	// An attempt that recorded nothing points at the stream rather than the
	// writer, so the next one fails fast instead of spending the writer's budget.
	retries := part.Retries
	if w.lastRecordingWasEmpty() {
		retries = 0
	}

	// Only Index/Path are known now; StartTime/EndTime/Bytes are filled in on
	// CurrentPart as the part records. The run loop appends the completed part
	// to w.output.Parts once those are set, so the manifest never reads a
	// zero-time part.
	w.output.CurrentPart = recording.Part{
		Index: w.output.PartIndex,
		Path:  part.Path,
	}

	// Built per part, so anything that must span parts is held by the worker
	// and handed back in every time.
	if w.carryover == nil {
		w.carryover = writer.NewCarryover()
	}
	w.writerCfg = &writer.Config{
		Carryover: w.carryover,
		StreamURL: w.selectedStream.URL,
		Headers:   w.downloadHeaders(),
		// The cloud-supplied UA (in downloadHeaders) wins in the writer; this is
		// only the floor when a stream carries no UA header. See internal/useragent.
		UserAgent: useragent.Fallback,
		Retries:   retries,
		Sanitize:  w.config.SanitizeFLV,
		// Names this build and session in the playlist the writer authors.
		Provenance: provenance.Record(w.output.WriterName, w.output.SessionID, partName),
		// Only used if the selected URL turns out to be a master playlist, so
		// the choice made here still holds one level down.
		Variant: w.config.Variant,
		// A platform that signs a stream URL for less than a part lasts gets it
		// re-resolved from under the writer, so the recording carries on across
		// the swap rather than stopping at the first 403.
		StreamURLTTL:     w.streamsTTL,
		RefreshStreamURL: w.refreshStreamURL,
		MouflonKeys:      w.str.Load().MouflonKeys,
		SegmentRename:    w.str.Load().SegmentRename,
		// The writer reports the part's codec config key on the recording
		// goroutine as the part finishes; recordGroupPart reads it right
		// after Record returns. The groups mutex covers the concurrent
		// activeGroupStats reader.
		OnConfigKey: func(key string) {
			w.groupsMu.Lock()
			w.curPartKey = key
			w.groupsMu.Unlock()
		},
	}

	return nil
}

// downloadHeaders builds the HTTP headers for the stream download: the cloud-
// provided headers (Referer/Origin) plus, when we hold a login for the
// streamer's platform AND the stream host belongs to that platform, the session
// cookies. Most platforms serve media from pre-signed URLs that need no
// cookies (so the host guard declines), but this keeps authenticated downloads
// working for platforms that serve media from their own cookie-scoped domains.
//
// PRIVACY: cookies are read from the local encrypted store, never logged, and —
// because of the AllowsHost guard — only ever sent to the platform's own hosts.
func (w *worker) downloadHeaders() map[string]string {
	str := w.str.Load()
	headers := make(map[string]string, len(str.Headers)+1)
	for k, v := range str.Headers {
		headers[k] = v
	}
	if w.cookies == nil || w.selectedStream == nil {
		return headers
	}
	if _, exists := headers["Cookie"]; exists {
		return headers // don't override cloud-provided auth
	}
	login := vlconfig.LoginFlowFor(str.Platform)
	if login == nil {
		return headers
	}
	u, err := url.Parse(w.selectedStream.URL)
	if err != nil || !login.AllowsHost(u.Host) {
		return headers
	}
	if sess, ok := w.cookies.Get(str.Platform); ok {
		headers["Cookie"] = sess.Header()
		w.logger.Info("Attached platform login to stream download",
			"platform", str.Platform, "host", u.Host, "cookie_count", len(sess.Cookies))
	}
	return headers
}

// logStreamVariants logs available stream variants to both the session logger and console
func (w *worker) logStreamVariants(streams stream.List) {
	// Log available streams to file
	if w.recAttempts > 0 {
		w.logger.Debug("Available streams", "count", len(streams))
	} else {
		w.logger.Info("Available streams", "count", len(streams))
	}
	for i, stream := range streams {
		// Check if this is the selected stream
		isSelected := w.selectedStream != nil &&
			stream.QualityID == w.selectedStream.QualityID &&
			stream.URL == w.selectedStream.URL

		// Determine position markers
		position := "mid"
		if i == 0 {
			position = "worst"
		} else if i == len(streams)-1 {
			position = "best"
		}

		logAttrs := []any{
			"index", i,
			"quality", stream.QualityID,
			"position", position,
			"selected", isSelected,
		}
		if stream.ResWidth > 0 && stream.ResHeight > 0 {
			logAttrs = append(logAttrs, "resolution", stream.Resolution())
		}
		logAttrs = append(logAttrs, "stream_url", stream.URL)
		if w.recAttempts > 0 {
			w.logger.Debug("Stream variant", logAttrs...)
		} else {
			w.logger.Info("Stream variant", logAttrs...)
		}
	}
}

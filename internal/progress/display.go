package progress

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/vodlabs-io/recorder/internal/recording"
)

// Column widths for the recording stats display line.
// Each accommodates the widest realistic value for that field:
//   - Label:    "[processing]" = 12 chars → 12
//   - Duration: "23h59m59s"    = 9 chars  → 10
//   - Total:    "999 MB"       = 6 chars  → 8
//   - Speed:    "1.0 MB/s"     = 8 chars  → 10
//   - Platform: "pandalive"    = 9 chars  → 10
const (
	colLabel    = 12
	colDuration = 10
	colTotal    = 8
	colSpeed    = 10
	colPlatform = 10
)

// StreamerRow holds the data needed to render one recording stats line.
type StreamerRow struct {
	Label    string
	Platform string
	Username string
	Stats    recording.Stats
}

// DisplayState is the full data needed to render the live status block.
type DisplayState struct {
	Uptime            time.Duration
	RecordingCount    int
	SubscriptionCount int
	Filtered          bool
	Rows              []StreamerRow
}

// formatRecordingLine formats a single streamer's stats as a fixed-width pipe-separated line.
func formatRecordingLine(row StreamerRow) string {
	dur := formatDuration(row.Stats.Duration.Round(time.Second))
	total := humanize.Bytes(uint64(row.Stats.TotalBytes))
	speed := humanize.Bytes(uint64(row.Stats.Speed)) + "/s"
	label := row.Label
	if label == "" {
		label = "recording"
	}
	return fmt.Sprintf("%-*s | %-*s | %-*s | %-*s | %-*s | %s",
		colLabel, "["+label+"]",
		colDuration, dur,
		colSpeed, speed,
		colTotal, total,
		colPlatform, row.Platform,
		row.Username)
}

// formatHeaderLine renders the top-of-display summary line.
//
// Under a filter the counts describe the whole daemon while the rows below
// describe the filter, so the filtered case says how many of them are shown
// rather than printing a total that does not match what is on screen.
func formatHeaderLine(s DisplayState) string {
	uptime := formatDuration(s.Uptime.Round(time.Second))
	if s.Filtered {
		if len(s.Rows) == 0 {
			return fmt.Sprintf("no streamer matches this filter (recording %d of %d)  uptime %s",
				s.RecordingCount, s.SubscriptionCount, uptime)
		}
		return fmt.Sprintf("showing %d of %d recording (%d tracked)  uptime %s",
			len(s.Rows), s.RecordingCount, s.SubscriptionCount, uptime)
	}
	if s.RecordingCount > 0 {
		return fmt.Sprintf("recording %d/%d  uptime %s",
			s.RecordingCount,
			s.SubscriptionCount,
			uptime)
	}
	return fmt.Sprintf("monitoring %d  uptime %s", s.SubscriptionCount, uptime)
}

// MultiLineDisplay renders a fixed block of lines to stdout, overwriting the previous
// block on each Render call so the output does not scroll. It is not safe for concurrent use.
type MultiLineDisplay struct {
	lastLineCount int
}

// Render overwrites the previously rendered lines with a header, optional
// separator, and one row per streamer in the given DisplayState.
// Rows are sorted by platform then username for a stable display order.
func (d *MultiLineDisplay) Render(state DisplayState) {
	d.clear()

	rows := state.Rows
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Platform != rows[j].Platform {
			return rows[i].Platform < rows[j].Platform
		}
		return rows[i].Username < rows[j].Username
	})

	header := formatHeaderLine(state)
	formatted := make([]string, 0, len(rows))
	maxLen := len(header)
	for _, row := range rows {
		line := formatRecordingLine(row)
		if len(line) > maxLen {
			maxLen = len(line)
		}
		formatted = append(formatted, line)
	}

	fmt.Println()
	fmt.Println(header)
	count := 2 // blank line + header
	if len(rows) > 0 {
		fmt.Println(strings.Repeat("-", maxLen))
		count++
	}
	for _, line := range formatted {
		fmt.Println(line)
		count++
	}
	d.lastLineCount = count
}

// PrintFinishedRow prints a static row above the live block. The live block
// is cleared first so the printed line ends up above it; the next Render call
// redraws the block underneath.
func (d *MultiLineDisplay) PrintFinishedRow(label string, stats recording.Stats, platform, username string) {
	d.clear()
	fmt.Println(formatRecordingLine(StreamerRow{
		Label:    label,
		Platform: platform,
		Username: username,
		Stats:    stats,
	}))
}

// printMessage prints a static message above the live block.
func (d *MultiLineDisplay) printMessage(message string) {
	d.clear()
	fmt.Println(message)
}

// clear erases the previously rendered live block.
func (d *MultiLineDisplay) clear() {
	if d.lastLineCount > 0 {
		fmt.Printf("\033[%dA\033[J", d.lastLineCount)
		d.lastLineCount = 0
	}
}

// display handles real-time single-line progress display for the daemon CLI.
type display struct {
	tracker        *Tracker
	ticker         *time.Ticker
	isRunning      bool
	lastLineLength int
	mu             sync.Mutex
	username       string
	platform       string
	workerCount    int
	totalSubs      int
}

// newDisplay creates a new progress display
func newDisplay(tracker *Tracker) *display {
	return &display{
		tracker: tracker,
	}
}

// Run begins the progress display with regular updates 4 times per second
func (pd *display) Run(ctx context.Context) {
	pd.mu.Lock()
	running := pd.isRunning
	if !running {
		pd.isRunning = true
		pd.ticker = time.NewTicker(250 * time.Millisecond)
	}
	pd.mu.Unlock()

	if running {
		return
	}

	for {
		select {
		case <-pd.ticker.C:
			pd.update()
		case <-ctx.Done():
			pd.stop()
			return
		}
	}
}

func (pd *display) setTracker(tracker *Tracker, platform, username string) {
	pd.mu.Lock()
	defer pd.mu.Unlock()
	pd.tracker = tracker
	pd.username = username
	pd.platform = platform
}

func (pd *display) setWorkerCount(count, total int) {
	pd.mu.Lock()
	defer pd.mu.Unlock()
	pd.workerCount = count
	pd.totalSubs = total
}

// stop freezes the progress display and clears the line for final stats output
func (pd *display) stop() {
	pd.mu.Lock()
	defer pd.mu.Unlock()

	if !pd.isRunning {
		return
	}

	pd.isRunning = false

	if pd.ticker != nil {
		pd.ticker.Stop()
	}

	// Clear the entire line first to remove any ^C characters
	fmt.Fprint(os.Stdout, "\r")
	for range 100 {
		fmt.Fprint(os.Stdout, " ")
	}
	fmt.Fprint(os.Stdout, "\r")
}

// update updates the progress display in real-time
func (pd *display) update() {
	pd.mu.Lock()
	defer pd.mu.Unlock()

	var progressLine string

	if pd.tracker == nil {
		progressLine = fmt.Sprintf("\r[monitoring] %d/%d", pd.workerCount, pd.totalSubs)
	} else {
		pd.tracker.AddSample()
		row := StreamerRow{
			Label:    "recording",
			Platform: pd.platform,
			Username: pd.username,
			Stats:    pd.tracker.GetStats(""),
		}
		progressLine = "\r" + formatRecordingLine(row)
	}

	pd.clearLine()
	fmt.Fprint(os.Stdout, progressLine)
	pd.lastLineLength = len(progressLine) - 1 // -1 for the \r
}

// clearLine clears the current terminal line
func (pd *display) clearLine() {
	if pd.lastLineLength > 0 {
		fmt.Fprint(os.Stdout, "\r")
		for i := 0; i < pd.lastLineLength; i++ {
			fmt.Fprint(os.Stdout, " ")
		}
		fmt.Fprint(os.Stdout, "\r")
	}
}

func (pd *display) printFinalStats(
	duration time.Duration,
	totalBytes int64,
	label, platform, username string,
) {
	progressLine := fmt.Sprintf("%-12s %-9s %-10s %s %s",
		"["+label+"]",
		formatDuration(duration),
		humanize.Bytes(uint64(totalBytes)),
		platform,
		username)

	pd.clearLine()
	fmt.Fprintln(os.Stdout, progressLine)
}

// formatDuration formats a duration into a compact human-readable string (e.g. 2h3m45s).
func formatDuration(d time.Duration) string {
	totalHours := int(d.Hours())
	days := totalHours / 24
	h := totalHours % 24
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd%dh%dm%ds", days, h, m, s)
	} else if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	} else if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

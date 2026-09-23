package progress

import (
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/recording"
)

// speedSample represents a single speed measurement
type speedSample struct {
	bytes int64
	time  time.Time
}

// Tracker handles progress tracking for recording operations
// It tracks bytes downloaded, calculates speed, and maintains timing information.
type Tracker struct {
	startTime  time.Time
	totalBytes int64
	mu         sync.RWMutex

	// Speed averaging over a sliding window
	speedSamples []speedSample
	maxSamples   int // Number of samples to keep for averaging
}

// NewTracker creates a new progress tracker
func NewTracker() *Tracker {
	now := time.Now()
	return &Tracker{
		startTime:    now,
		maxSamples:   10, // Keep 10 samples for 5-second sliding window at 0.5s intervals
		speedSamples: make([]speedSample, 0, 10),
	}
}

// UpdateBytes updates the total bytes recorded and records a speed sample
// The bytes parameter should be the increment added since last update
func (pt *Tracker) UpdateBytes(bytesIncrement int64) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := time.Now()
	pt.totalBytes += bytesIncrement

	// Add new speed sample
	sample := speedSample{
		bytes: pt.totalBytes,
		time:  now,
	}

	pt.speedSamples = append(pt.speedSamples, sample)

	// Keep only the most recent samples within our window
	if len(pt.speedSamples) > pt.maxSamples {
		pt.speedSamples = pt.speedSamples[1:]
	}
}

// AddSample adds a time-based sample for regular updates
// This ensures we have regular samples even when bytes aren't changing
func (pt *Tracker) AddSample() {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := time.Now()
	sample := speedSample{
		bytes: pt.totalBytes,
		time:  now,
	}
	pt.speedSamples = append(pt.speedSamples, sample)

	// Keep only the most recent samples within our window
	if len(pt.speedSamples) > pt.maxSamples*4 { // Allow more samples since we're adding them frequently
		// Remove samples older than 5 seconds
		windowStart := now.Add(-5 * time.Second)
		validSamples := make([]speedSample, 0, len(pt.speedSamples))
		for _, s := range pt.speedSamples {
			if s.time.After(windowStart) {
				validSamples = append(validSamples, s)
			}
		}
		pt.speedSamples = validSamples
	}
}

// GetStats returns the current statistics snapshot
func (pt *Tracker) GetStats(sessionID string) recording.Stats {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	now := time.Now()
	return recording.Stats{
		TotalBytes: pt.totalBytes,
		Duration:   now.Sub(pt.startTime),
		Speed:      pt.calculateAverageSpeed(now),
		SessionID:  sessionID,
	}
}

// calculateAverageSpeed computes the average speed over the sliding window
// Must be called with lock held
func (pt *Tracker) calculateAverageSpeed(now time.Time) float64 {
	if len(pt.speedSamples) < 2 {
		return 0.0
	}

	// Remove samples older than 5 seconds
	windowStart := now.Add(-5 * time.Second)
	validSamples := make([]speedSample, 0, len(pt.speedSamples))

	for _, sample := range pt.speedSamples {
		if sample.time.After(windowStart) {
			validSamples = append(validSamples, sample)
		}
	}

	if len(validSamples) < 2 {
		return 0.0
	}

	// Calculate speed using earliest and latest samples in window
	earliest := validSamples[0]
	latest := validSamples[len(validSamples)-1]

	timeDelta := latest.time.Sub(earliest.time).Seconds()
	if timeDelta <= 0 {
		return 0.0
	}

	bytesDelta := latest.bytes - earliest.bytes

	// If no bytes changed, it means we're in a gap between segments
	// Return 0 to accurately reflect no current transfer
	if bytesDelta == 0 {
		return 0.0
	}

	// Calculate actual transfer speed over the time period where data was received
	// This gives us the true download speed, not averaged over idle time
	return float64(bytesDelta) / timeDelta
}

// GetOverallAverageSpeed returns the average speed over the entire recording duration
func (pt *Tracker) GetOverallAverageSpeed() float64 {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	duration := time.Since(pt.startTime).Seconds()
	if duration <= 0 {
		return 0.0
	}

	return float64(pt.totalBytes) / duration
}

package progress

import (
	"testing"
	"time"
)

// A tracker for a resumed session reads as though recording never stopped:
// elapsed time from the original start, and bytes on top of what was kept.
func TestNewTrackerFromCarriesOn(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	tr := NewTrackerFrom(start, 1000)
	tr.UpdateBytes(500)

	stats := tr.GetStats("sess")
	if stats.TotalBytes != 1500 {
		t.Fatalf("TotalBytes = %d, want 1500", stats.TotalBytes)
	}
	if stats.Duration < time.Hour {
		t.Fatalf("Duration = %v, want at least an hour", stats.Duration)
	}
	if speed := tr.GetOverallAverageSpeed(); speed <= 0 || speed > 1 {
		t.Fatalf("overall speed = %v B/s, want 1500 bytes over an hour", speed)
	}
}

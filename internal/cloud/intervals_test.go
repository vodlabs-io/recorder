package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func intP(n int) *int { return &n }

// TestGetIntervalsCache verifies the cache serves within the TTL and refetches
// once the entry goes stale.
func TestGetIntervalsCache(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	calls := 0
	resp1 := &intervalsResponse{
		TargetSeconds: 300,
		Intervals:     []platformInterval{{Platform: "tiktok", EstimatedSeconds: intP(120)}},
	}
	fetch := func(context.Context) (*intervalsResponse, error) {
		calls++
		return resp1, nil
	}

	got, err := c.getIntervals(ctx, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resp1 || calls != 1 {
		t.Fatalf("first call: got=%p calls=%d, want resp1 and 1 call", got, calls)
	}

	// Within TTL: served from cache, no new fetch.
	if _, err := c.getIntervals(ctx, fetch); err != nil {
		t.Fatalf("cached call error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (cache hit)", calls)
	}

	// Force the cache stale: next call must refetch.
	c.intervals.fetched = time.Now().Add(-2 * intervalsCacheTTL)
	resp2 := &intervalsResponse{TargetSeconds: 300}
	got, err = c.getIntervals(ctx, func(context.Context) (*intervalsResponse, error) {
		calls++
		return resp2, nil
	})
	if err != nil {
		t.Fatalf("refetch error: %v", err)
	}
	if got != resp2 || calls != 2 {
		t.Fatalf("stale refetch: got=%p calls=%d, want resp2 and 2 calls", got, calls)
	}
}

// TestGetIntervalsStaleFallback verifies a stale cache is returned when a refetch
// fails, rather than surfacing the error.
func TestGetIntervalsStaleFallback(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	c.intervals.data = &intervalsResponse{TargetSeconds: 300}
	c.intervals.fetched = time.Now().Add(-2 * intervalsCacheTTL)

	got, err := c.getIntervals(ctx, func(context.Context) (*intervalsResponse, error) {
		return nil, errors.New("boom")
	})
	if err != nil {
		t.Fatalf("expected stale fallback, got error: %v", err)
	}
	if got != c.intervals.data {
		t.Fatalf("expected stale cache to be returned")
	}
}

// TestGetIntervalsErrorNoCache verifies the error surfaces when there is no cache
// to fall back on.
func TestGetIntervalsErrorNoCache(t *testing.T) {
	c := testClient(t)
	_, err := c.getIntervals(context.Background(), func(context.Context) (*intervalsResponse, error) {
		return nil, errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error when no cache is available")
	}
}

// TestIntervalsResponseDecode confirms the wire shape decodes, including null
// estimate/updated_at fields.
func TestIntervalsResponseDecode(t *testing.T) {
	j := `{"target_seconds":300,"intervals":[` +
		`{"platform":"tiktok","estimated_seconds":272,"updated_at":1768229000},` +
		`{"platform":"pandalive","estimated_seconds":null,"updated_at":null}]}`

	var r intervalsResponse
	if err := json.Unmarshal([]byte(j), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.TargetSeconds != 300 {
		t.Errorf("target_seconds = %d, want 300", r.TargetSeconds)
	}
	if len(r.Intervals) != 2 {
		t.Fatalf("intervals len = %d, want 2", len(r.Intervals))
	}
	if r.Intervals[0].EstimatedSeconds == nil || *r.Intervals[0].EstimatedSeconds != 272 {
		t.Errorf("tiktok estimated_seconds = %v, want 272", r.Intervals[0].EstimatedSeconds)
	}
	if r.Intervals[1].EstimatedSeconds != nil || r.Intervals[1].UpdatedAt != nil {
		t.Errorf("pandalive should have nil estimate/updated_at, got %+v", r.Intervals[1])
	}
}

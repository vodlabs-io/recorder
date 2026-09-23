package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vodlabs-io/recorder/internal/flvfix"
	"github.com/vodlabs-io/recorder/internal/vlerror"
)

// TestReconnectPlan pins how quickly a dropped stream comes back: a refused
// reconnect asks for a fresh URL, and a part that was recording skips the lag
// delay.
func TestReconnectPlan(t *testing.T) {
	refused := fmt.Errorf("%w: server returned status 503", vlerror.ErrConnectFailed)
	for _, tc := range []struct {
		name          string
		err           error
		bytes         int64
		wantFresh     bool
		wantSkipDelay bool
	}{
		{"drop after recording", refused, 5 << 20, true, true},
		{"still refused on the retry", refused, 0, true, false},
		{"part stopped without a connect failure", nil, 5 << 20, false, false},
		{"configuration change", flvfix.ErrConfigChange, 5 << 20, false, false},
		{"disk failure", vlerror.ErrWriteFailed, 5 << 20, false, false},
		{"stream read errors", errors.New("stream failed after 2 consecutive errors"), 5 << 20, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh, skip := reconnectPlan(tc.err, tc.bytes)
			if fresh != tc.wantFresh || skip != tc.wantSkipDelay {
				t.Errorf("reconnectPlan = (fresh %v, skip delay %v), want (%v, %v)",
					fresh, skip, tc.wantFresh, tc.wantSkipDelay)
			}
		})
	}
}

func TestStreamHostDropsSigning(t *testing.T) {
	got := streamHost("https://pull-flv-f77-sg01.tiktokcdn-us.com/stage/stream-1_or4.flv?expire=1&sign=abc")
	if got != "pull-flv-f77-sg01.tiktokcdn-us.com" {
		t.Errorf("streamHost = %q", got)
	}
}

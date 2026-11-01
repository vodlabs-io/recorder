package encoding

import (
	"math"
	"strings"
	"testing"
)

func TestParseEncodeProgressKnownDuration(t *testing.T) {
	// Two progress blocks; the "end" block forces a final report.
	stream := strings.Join([]string{
		"frame=1",
		"out_time_us=5000000",
		"speed=2.0x",
		"progress=continue",
		"out_time_us=10000000",
		"speed=2.5x",
		"progress=end",
		"",
	}, "\n")

	var lastPercent, lastSpeed float64
	var lastEta int64
	var calls int
	parseEncodeProgress(strings.NewReader(stream), 0, 20.0, func(percent float64, eta int64, speed float64) {
		calls++
		lastPercent, lastEta, lastSpeed = percent, eta, speed
	})

	if calls == 0 {
		t.Fatal("progress never reported")
	}
	if math.Abs(lastPercent-50) > 0.01 {
		t.Errorf("percent = %.2f, want 50", lastPercent)
	}
	if math.Abs(lastSpeed-2.5) > 0.01 {
		t.Errorf("speed = %.2f, want 2.5", lastSpeed)
	}
	// eta = (20 - 10) / 2.5 = 4
	if lastEta != 4 {
		t.Errorf("eta = %d, want 4", lastEta)
	}
}

func TestParseEncodeProgressCapsAt100(t *testing.T) {
	stream := "out_time_us=30000000\nspeed=1.0x\nprogress=end\n"
	var last float64
	parseEncodeProgress(strings.NewReader(stream), 0, 20.0, func(percent float64, _ int64, _ float64) {
		last = percent
	})
	if last != 100 {
		t.Errorf("percent = %.2f, want capped 100", last)
	}
}

// A batch of a longer merge reports progress through the whole merge.
func TestParseEncodeProgressOffset(t *testing.T) {
	stream := "out_time_us=10000000\nspeed=2.0x\nprogress=end\n"
	var percent float64
	var eta int64
	parseEncodeProgress(strings.NewReader(stream), 30, 100, func(p float64, e int64, _ float64) {
		percent, eta = p, e
	})
	// (30 + 10) of 100, and (100 - 40) / 2 to go.
	if math.Abs(percent-40) > 0.01 || eta != 30 {
		t.Errorf("percent = %.2f eta = %d, want 40 and 30", percent, eta)
	}
}

func TestParseEncodeProgressIndeterminate(t *testing.T) {
	stream := "out_time_us=5000000\nspeed=2.0x\nprogress=end\n"
	var last float64 = 999
	parseEncodeProgress(strings.NewReader(stream), 0, 0, func(percent float64, _ int64, _ float64) {
		last = percent
	})
	if last != -1 {
		t.Errorf("percent = %.2f, want -1 (indeterminate) when total unknown", last)
	}
}

func TestParseEncodeProgressClockFallback(t *testing.T) {
	// out_time (clock form) is the last-resort field.
	stream := "out_time=00:00:15.000000\nspeed=1.0x\nprogress=end\n"
	var last float64
	parseEncodeProgress(strings.NewReader(stream), 0, 30.0, func(percent float64, _ int64, _ float64) {
		last = percent
	})
	if math.Abs(last-50) > 0.01 {
		t.Errorf("percent = %.2f, want 50 from clock time", last)
	}
}

func TestParseClockTime(t *testing.T) {
	if got, ok := parseClockTime("01:02:03.5"); !ok || math.Abs(got-3723.5) > 0.001 {
		t.Errorf("parseClockTime = %.3f, %v; want 3723.5, true", got, ok)
	}
	if _, ok := parseClockTime("1:2"); ok {
		t.Errorf("parseClockTime(\"1:2\") should fail")
	}
	if _, ok := parseClockTime("aa:bb:cc"); ok {
		t.Errorf("parseClockTime with non-numeric should fail")
	}
}

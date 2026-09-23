package vlconfig

import (
	"testing"
	"time"
)

func TestParseSplitSize(t *testing.T) {
	tests := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"off", 0, false},
		{"OFF", 0, false},
		{" off ", 0, false},
		{"0", 0, false},
		{"10MB", 10_000_000, false}, // exactly the floor passes
		{"2GB", 2_000_000_000, false},
		{"500MB", 500_000_000, false},
		{"1.5GB", 1_500_000_000, false},
		{"5MB", 0, true}, // below floor
		{"9MB", 0, true}, // below floor
		{"banana", 0, true},
		{"nonsense", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseSplitSize(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseSplitSize(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ParseSplitSize(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseSplitDuration(t *testing.T) {
	tests := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"off", 0, false},
		{"0", 0, false},
		{"1m", time.Minute, false}, // exactly the floor passes
		{"60s", time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"45m", 45 * time.Minute, false},
		{"30s", 0, true}, // below floor
		{"59s", 0, true}, // below floor
		{"-5m", 0, true}, // negative
		{"banana", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseSplitDuration(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseSplitDuration(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ParseSplitDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestIsSplitOff(t *testing.T) {
	on := []string{"", "2GB", "1h", "on", "10MB"}
	off := []string{"off", "OFF", "Off", "0", " off ", " 0 "}
	for _, s := range on {
		if IsSplitOff(s) {
			t.Errorf("IsSplitOff(%q) = true, want false", s)
		}
	}
	for _, s := range off {
		if !IsSplitOff(s) {
			t.Errorf("IsSplitOff(%q) = false, want true", s)
		}
	}
}

func TestSplitLimitsEnabled(t *testing.T) {
	tests := []struct {
		l    splitLimits
		want bool
	}{
		{splitLimits{}, false},
		{splitLimits{Size: 100}, true},
		{splitLimits{Duration: time.Minute}, true},
		{splitLimits{Size: 100, Duration: time.Minute}, true},
	}
	for _, tc := range tests {
		if got := tc.l.Enabled(); got != tc.want {
			t.Errorf("%+v Enabled() = %v, want %v", tc.l, got, tc.want)
		}
	}
}

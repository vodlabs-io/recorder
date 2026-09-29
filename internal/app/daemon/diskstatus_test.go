package daemon

import (
	"slices"
	"testing"
)

func TestRecoveredPaths(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		want                []string
	}{
		{"none were down", "", "/a", nil},
		{"all back", "/a\x00/b", "", []string{"/a", "/b"}},
		{"one back", "/a\x00/b", "/b", []string{"/a"}},
		{"still down", "/a", "/a", nil},
	} {
		if got := recoveredPaths(tc.before, tc.after); !slices.Equal(got, tc.want) {
			t.Errorf("%s: recoveredPaths = %q, want %q", tc.name, got, tc.want)
		}
	}
}

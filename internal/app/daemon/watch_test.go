package daemon

import "testing"

func TestStreamerFileChange(t *testing.T) {
	canonical := [32]byte{1} // what the daemon last wrote
	external := [32]byte{2}  // a user's host edit
	rewritten := [32]byte{3} // daemon's own subsequent normalize/purge write

	tests := []struct {
		name             string
		h, seen, written [32]byte
		wantSeen         [32]byte
		wantResubscribe  bool
	}{
		{"no change", canonical, canonical, canonical, canonical, false},
		{"external edit fires", external, canonical, canonical, external, true},
		{"same content after firing is deduped", external, external, canonical, external, false},
		{"daemon own write ignored", rewritten, canonical, rewritten, rewritten, false},
		{"daemon write after external edit ignored", rewritten, external, rewritten, rewritten, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen, resub := streamerFileChange(tt.h, tt.seen, tt.written)
			if seen != tt.wantSeen {
				t.Errorf("newSeen = %v, want %v", seen, tt.wantSeen)
			}
			if resub != tt.wantResubscribe {
				t.Errorf("resubscribe = %v, want %v", resub, tt.wantResubscribe)
			}
		})
	}
}

package vlapi

import (
	"testing"

	"github.com/vodlabs-io/recorder/internal/platformauth"
)

func TestKeepHealth(t *testing.T) {
	jar := func(token string) []platformauth.Cookie {
		return []platformauth.Cookie{{Name: "token", Value: token}, {Name: "other", Value: "x"}}
	}
	expired := platformauth.Session{Cookies: jar("a"), Healthy: false, LastCheckedAt: 100}

	same := platformauth.Session{Cookies: jar("a"), Healthy: true}
	keepHealth(&same, expired, "token")
	if same.Healthy || same.LastCheckedAt != 100 {
		t.Errorf("re-stored jar = %+v, want the expired health kept", same)
	}

	fresh := platformauth.Session{Cookies: jar("b"), Healthy: true}
	keepHealth(&fresh, expired, "token")
	if !fresh.Healthy {
		t.Error("a new session cookie kept the old login's expired health")
	}

	noScheme := platformauth.Session{Cookies: jar("a"), Healthy: true}
	keepHealth(&noScheme, expired, "")
	if !noScheme.Healthy {
		t.Error("a platform with no session cookie kept the old health")
	}
}

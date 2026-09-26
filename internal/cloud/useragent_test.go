package cloud

import (
	"fmt"
	"runtime"
	"testing"
)

func TestClientUserAgent(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"", "standalone"},
		{"desktop", "desktop"},
		{"cli", "cli"},
		{"Desktop", "standalone"},
		{"desk top", "standalone"},
		{"averyveryverylongname", "standalone"},
	} {
		t.Setenv(launcherEnv, c.env)
		want := fmt.Sprintf("vodlabs-recorder/1.2.3 (%s; %s; %s)", runtime.GOOS, runtime.GOARCH, c.want)
		if got := clientUserAgent("1.2.3"); got != want {
			t.Errorf("%s=%q: got %q, want %q", launcherEnv, c.env, got, want)
		}
	}
}

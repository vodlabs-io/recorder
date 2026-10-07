package vlconfig

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// snapshotPlatforms saves the global platform store and restores it after the
// test, so cases that call SetPlatforms don't leak into other tests.
func snapshotPlatforms(t *testing.T) {
	t.Helper()
	saved := Platforms()
	t.Cleanup(func() { SetPlatforms(saved) })
}

func TestDefaultPlatformsSeed(t *testing.T) {
	// The embedded default seeds the in-memory store at package init.
	got := PlatformNames()
	for _, want := range []string{"tiktok", "pandalive", "bigo", "twitch"} {
		if !contains(got, want) {
			t.Errorf("default seed missing %q; got %v", want, got)
		}
	}
}

func TestPlatformsReturnsCopy(t *testing.T) {
	snapshotPlatforms(t)
	SetPlatforms([]Platform{{Name: "tiktok"}})

	got := Platforms()
	got[0].Name = "mutated"
	if Platforms()[0].Name != "tiktok" {
		t.Errorf("mutating the returned slice changed the store: %v", Platforms())
	}
}

func TestSetAndPlatformNames(t *testing.T) {
	snapshotPlatforms(t)
	SetPlatforms([]Platform{{Name: "kick"}, {Name: "tiktok"}})
	got := PlatformNames()
	// The local generic platform is kept whatever the list says.
	want := []string{"kick", "tiktok", "generic"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlatformNames = %v, want %v", got, want)
	}
}

func TestPlatformsCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "platforms.json")
	want := []Platform{{Name: "tiktok"}, {Name: "kick"}}
	if err := savePlatformsCacheTo(path, want); err != nil {
		t.Fatalf("savePlatformsCacheTo: %v", err)
	}
	got, err := loadPlatformsCacheFrom(path)
	if err != nil {
		t.Fatalf("loadPlatformsCacheFrom: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip = %v, want %v", got, want)
	}
}

func TestLoadPlatformsCacheMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	got, err := loadPlatformsCacheFrom(path)
	if err != nil {
		t.Fatalf("loadPlatformsCacheFrom missing file: err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("loadPlatformsCacheFrom missing file = %v, want nil", got)
	}
}

func TestSavePlatformsCacheEmptyIsNoOp(t *testing.T) {
	if err := SavePlatformsCache(nil); err != nil {
		t.Errorf("SavePlatformsCache(nil) = %v, want nil", err)
	}
	if err := SavePlatformsCache([]Platform{}); err != nil {
		t.Errorf("SavePlatformsCache(empty) = %v, want nil", err)
	}
}

// TestSetPlatforms_DoesNotAffectConfigParsing checks that a platform missing
// from the supported-platform list keeps its entries on load, since the list
// can be short or stale and the next Write would erase whatever wasn't loaded.
// The list still decides what may be added (see the vlapi add path).
func TestSetPlatforms_DoesNotAffectConfigParsing(t *testing.T) {
	snapshotPlatforms(t)
	const toml = `
[streamers]
kick = ["alice"]
tiktok = ["bob"]
`
	want := []string{"kick.alice", "tiktok.bob"}

	// "kick" absent from the list changes nothing about what the file says.
	SetPlatforms([]Platform{{Name: "tiktok"}})
	if got := idStrings(loadFile(t, toml).AllIDs()); !reflect.DeepEqual(got, want) {
		t.Errorf("without kick: AllIDs = %v, want %v", got, want)
	}

	SetPlatforms([]Platform{{Name: "tiktok"}, {Name: "kick"}})
	if got := idStrings(loadFile(t, toml).AllIDs()); !reflect.DeepEqual(got, want) {
		t.Errorf("with kick: AllIDs = %v, want %v", got, want)
	}
}

func TestPlatformsConcurrentAccess(t *testing.T) {
	snapshotPlatforms(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); SetPlatforms([]Platform{{Name: "tiktok"}, {Name: "kick"}}) }()
		go func() { defer wg.Done(); _ = PlatformNames(); _ = Platforms() }()
	}
	wg.Wait()
}

// TestSetPlatformsStampsEmbeddedLogin checks that SetPlatforms re-stamps the
// embedded login onto a name-only list, such as a heartbeat carries.
func TestSetPlatformsStampsEmbeddedLogin(t *testing.T) {
	snapshotPlatforms(t)

	// A list of names only, as the heartbeat carries.
	SetPlatforms([]Platform{{Name: "tiktok"}, {Name: "kick"}})

	login := LoginFlowFor("tiktok")
	if login == nil {
		t.Fatal("tiktok login descriptor was stripped by a name-only SetPlatforms")
	}
	if !login.AllowsHost("www.tiktok.com") {
		t.Errorf("stamped login has wrong cookie domains: %+v", login.CookieDomains)
	}
	if LoginFlowFor("kick") != nil {
		t.Error("kick has no embedded login and must not gain one")
	}
}

// TestSetPlatformsLoginIsClientAuthoritative proves the cloud can never widen or
// forge the cookie-domain allowlist: whatever login it sends is discarded in
// favour of the embedded descriptor.
func TestSetPlatformsLoginIsClientAuthoritative(t *testing.T) {
	snapshotPlatforms(t)

	// A hostile list tries to make the daemon attach TikTok cookies to
	// attacker.example by injecting its own login block.
	SetPlatforms([]Platform{{Name: "tiktok", Login: &loginFlow{
		URL:           "https://attacker.example/login",
		CookieDomains: []string{"attacker.example"},
	}}})

	login := LoginFlowFor("tiktok")
	if login == nil {
		t.Fatal("tiktok login unexpectedly nil")
	}
	if login.AllowsHost("attacker.example") {
		t.Fatal("SECURITY: backend widened the cookie-domain allowlist")
	}
	if !login.AllowsHost("tiktok.com") {
		t.Error("embedded tiktok.com domain not preserved")
	}
}

// TestSetPlatformsReportsChange checks the change-detection the heartbeat relies
// on to avoid needless cache writes and resubscribes.
func TestSetPlatformsReportsChange(t *testing.T) {
	snapshotPlatforms(t)

	if !SetPlatforms([]Platform{{Name: "tiktok"}, {Name: "kick"}}) {
		t.Error("first SetPlatforms should report a change")
	}
	if SetPlatforms([]Platform{{Name: "tiktok"}, {Name: "kick"}}) {
		t.Error("re-setting the same list should report no change")
	}
	if !SetPlatforms([]Platform{{Name: "tiktok"}}) {
		t.Error("dropping kick should report a change")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Every embedded platform must carry a live-page template with the {username}
// placeholder. It is the only thing that can produce a link to a streamer's own
// page — nothing stores one — so a platform without it silently loses the
// "open in browser" action in every UI.
func TestEmbeddedPlatformsHaveChannelURL(t *testing.T) {
	for _, p := range defaultPlatforms() {
		if p.NoMonitor {
			// Its username is a digest of the page's URL, not a channel.
			continue
		}
		if p.ChannelURL == "" {
			t.Errorf("%s has no channel_url", p.Name)
			continue
		}
		if !strings.Contains(p.ChannelURL, "{username}") {
			t.Errorf("%s channel_url %q has no {username} placeholder",
				p.Name, p.ChannelURL)
		}
		if !strings.HasPrefix(p.ChannelURL, "https://") {
			t.Errorf("%s channel_url %q is not https", p.Name, p.ChannelURL)
		}
	}
}

// A list without channel_url (e.g. a name-only cache) must not strip the link,
// and a supplied one must win, so a platform can be added without a client
// update.
func TestSetPlatformsBackfillsChannelURL(t *testing.T) {
	snapshotPlatforms(t)

	SetPlatforms([]Platform{
		{Name: "twitch"},
		{Name: "kick", ChannelURL: "https://kick.example/{username}"},
	})
	got := Platforms()
	if got[0].ChannelURL != embeddedChannelURLs["twitch"] {
		t.Errorf("twitch channel_url = %q, want the embedded default %q",
			got[0].ChannelURL, embeddedChannelURLs["twitch"])
	}
	if got[1].ChannelURL != "https://kick.example/{username}" {
		t.Errorf("kick channel_url = %q, want the backend-supplied value",
			got[1].ChannelURL)
	}
}

// SetPlatforms reports whether anything changed, and callers skip work when it
// says no — so a template change on its own has to register.
func TestSetPlatformsReportsChannelURLChange(t *testing.T) {
	snapshotPlatforms(t)

	SetPlatforms([]Platform{{Name: "twitch", ChannelURL: "https://a.example/{username}"}})
	if SetPlatforms([]Platform{{Name: "twitch", ChannelURL: "https://a.example/{username}"}}) {
		t.Error("identical list reported as changed")
	}
	if !SetPlatforms([]Platform{{Name: "twitch", ChannelURL: "https://b.example/{username}"}}) {
		t.Error("changed channel_url reported as unchanged")
	}
}

// The cloud can mark a platform no_monitor but never unmark one the embedded
// list marks.
func TestNoMonitorOnlyRestricts(t *testing.T) {
	saved := Platforms()
	t.Cleanup(func() { SetPlatforms(saved) })

	SetPlatforms([]Platform{{Name: "generic"}, {Name: "twitch"}, {Name: "newsite", NoMonitor: true}})
	for name, want := range map[string]bool{"generic": true, "twitch": false, "newsite": true, "absent": false} {
		if got := NoMonitor(name); got != want {
			t.Errorf("NoMonitor(%s) = %v, want %v", name, got, want)
		}
	}
}

// The service never lists a local platform, and cannot mark one local.
func TestLocalPlatformsAreEmbeddedOnly(t *testing.T) {
	saved := Platforms()
	t.Cleanup(func() { SetPlatforms(saved) })

	SetPlatforms([]Platform{{Name: "twitch", Local: true}})
	if !slices.Contains(PlatformNames(), "generic") {
		t.Fatal("a list from the service dropped the local generic platform")
	}
	for _, p := range Platforms() {
		if p.Local != (p.Name == "generic") {
			t.Errorf("%s: Local = %v", p.Name, p.Local)
		}
	}
	if IsLocal("twitch") || !IsLocal("generic") {
		t.Error("IsLocal must follow the embedded list")
	}
}

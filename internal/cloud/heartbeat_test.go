package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// testClient builds a minimal Client sufficient for applyPlatforms: a no-op
// logger and a buffered resubscribe channel.
func testClient(t *testing.T) *Client {
	t.Helper()
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "info"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	return &Client{
		logger:        log,
		ResubscribeCh: make(chan struct{}, 1),
		ReloadCh:      make(chan struct{}, 1),
	}
}

func resubscribed(c *Client) bool {
	select {
	case <-c.ResubscribeCh:
		return true
	default:
		return false
	}
}

func reloadQueued(c *Client) bool {
	select {
	case <-c.ReloadCh:
		return true
	default:
		return false
	}
}

func TestApplyPlatforms(t *testing.T) {
	// Must be VLREC_CACHE_DIR, not XDG_CACHE_HOME: os.UserCacheDir ignores the
	// XDG variable on macOS, so the test would overwrite the developer's real
	// platforms.json.
	t.Setenv(vlconfig.CacheDirEnv, t.TempDir())
	saved := vlconfig.Platforms()
	t.Cleanup(func() { vlconfig.SetPlatforms(saved) })

	c := testClient(t)

	// A changed list updates the store, persists the cache, and resubscribes.
	want := []vlconfig.Platform{{Name: "tiktok"}, {Name: "kick"}}
	c.applyPlatforms(want)
	if got := vlconfig.PlatformNames(); !reflect.DeepEqual(got, []string{"tiktok", "kick"}) {
		t.Fatalf("after apply: PlatformNames = %v, want [tiktok kick]", got)
	}
	cached, err := vlconfig.LoadPlatformsCache()
	if err != nil {
		t.Fatalf("LoadPlatformsCache: %v", err)
	}
	if !reflect.DeepEqual(cached, want) {
		t.Errorf("cache = %v, want %v", cached, want)
	}
	if !resubscribed(c) {
		t.Error("expected resubscribe to be triggered on change")
	}

	// An unchanged list is a no-op (no resubscribe).
	c.applyPlatforms(want)
	if resubscribed(c) {
		t.Error("unexpected resubscribe on unchanged list")
	}

	// An empty list (e.g. a response missing the field) is ignored.
	c.applyPlatforms(nil)
	if got := vlconfig.PlatformNames(); !reflect.DeepEqual(got, []string{"tiktok", "kick"}) {
		t.Errorf("empty list changed platforms to %v", got)
	}
	if resubscribed(c) {
		t.Error("unexpected resubscribe on empty list")
	}
}

// TestHeartbeatResponseDecode confirms the wire shape served by the cloud API
// (version fields + supported_platforms objects) decodes into our local struct,
// with version.Upstream fields still populated via embedding.
func TestHeartbeatResponseDecode(t *testing.T) {
	srvJSON := `{
		"minimum_version": "0.0.3",
		"maximum_version": "0.0.3",
		"recommended_version": "0.0.3",
		"supported_platforms": [{"name":"tiktok"},{"name":"kick"}]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(srvJSON))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var result heartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Upstream != (version.Upstream{MinimumVersion: "0.0.3", MaximumVersion: "0.0.3", RecommendedVersion: "0.0.3"}) {
		t.Errorf("version fields = %+v", result.Upstream)
	}
	names := make([]string, len(result.SupportedPlatforms))
	for i, p := range result.SupportedPlatforms {
		names[i] = p.Name
	}
	if !slices.Equal(names, []string{"tiktok", "kick"}) {
		t.Errorf("supported_platforms = %v, want [tiktok kick]", names)
	}
}

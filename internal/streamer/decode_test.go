package streamer

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestDecodeCloudStatus pins the field names on the wire. The cloud API sends
// snake_case, and Go's fallback matching for an untagged field is
// case-insensitive but not underscore-insensitive — so a missing json tag here
// does not fail loudly, it silently decodes to the zero value and the daemon
// carries an empty name around forever.
func TestDecodeCloudStatus(t *testing.T) {
	const payload = `{
		"platform": "chzzk",
		"username": "bb382c2c0cc9fa7c86ab3b037fb5799c",
		"display_name": "Example Streamer",
		"status": "live",
		"avatar_url": "https://img.example/a.jpg",
		"live_metadata": {
			"platform": "chzzk",
			"title": "제목",
			"display_name": "Example Streamer",
			"platform_user_id": "bb382c2c0cc9fa7c86ab3b037fb5799c",
			"live_type": "talk"
		}
	}`

	var got StreamerState
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	checks := []struct{ field, got, want string }{
		{"Platform", got.Platform, "chzzk"},
		{"Username", got.Username, "bb382c2c0cc9fa7c86ab3b037fb5799c"},
		{"DisplayName", got.DisplayName, "Example Streamer"},
		{"AvatarURL", got.AvatarURL, "https://img.example/a.jpg"},
		{"LiveMetadata.Platform", got.LiveMetadata.Platform, "chzzk"},
		{"LiveMetadata.Title", got.LiveMetadata.Title, "제목"},
		{"LiveMetadata.DisplayName", got.LiveMetadata.DisplayName, "Example Streamer"},
		{"LiveMetadata.PlatformUserID", got.LiveMetadata.PlatformUserID, "bb382c2c0cc9fa7c86ab3b037fb5799c"},
		{"LiveMetadata.LiveType", got.LiveMetadata.LiveType, "talk"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if got.Status != StatusLive {
		t.Errorf("Status = %q, want %q", got.Status, StatusLive)
	}
}

// TestDecodeLiveMetadataFieldNames pins every live_metadata key the cloud sends.
//
// The reflective sweep is the point: a field added to LiveMetadata without a
// json tag, or with a tag that doesn't match what the backend marshals, decodes
// silently to its zero value. Leaving one zero here fails the test, so a new
// field cannot be added to the struct without a payload key to feed it.
func TestDecodeLiveMetadataFieldNames(t *testing.T) {
	const payload = `{
		"platform": "twitch",
		"title": "guessing your rank",
		"display_name": "Streamer Four",
		"platform_user_id": "411377640",
		"live_type": "live",
		"room_id": "321206750681",
		"category": "Rainbow Six Siege",
		"language": "en",
		"tags": ["English", "DropsEnabled"],
		"viewers": 23783,
		"started_at": 1788547066,
		"thumbnail_url": "https://static-cdn.example/live_user_x-1920x1080.jpg",
		"adult": true,
		"extra": {"stream_id": "321206750681"}
	}`

	var got LiveMetadata
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	v := reflect.ValueOf(got)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if v.Field(i).IsZero() {
			t.Errorf("LiveMetadata.%s decoded to its zero value: the payload key "+
				"above and the field's json tag do not match", name)
		}
	}

	if got.Title != "guessing your rank" || got.Category != "Rainbow Six Siege" {
		t.Errorf("decoded wrong values: title=%q category=%q", got.Title, got.Category)
	}
	if len(got.Tags) != 2 || got.Viewers != 23783 || !got.Adult {
		t.Errorf("decoded wrong values: tags=%v viewers=%d adult=%v", got.Tags, got.Viewers, got.Adult)
	}
}

func TestLiveMetadataDiffers(t *testing.T) {
	base := LiveMetadata{Title: "one", Category: "IRL", Tags: []string{"a"}, Viewers: 10}

	tests := []struct {
		name string
		mod  func(*LiveMetadata)
		want bool
	}{
		{"identical", func(*LiveMetadata) {}, false},
		{"viewer count only", func(m *LiveMetadata) { m.Viewers = 999 }, false},
		{"title changed", func(m *LiveMetadata) { m.Title = "two" }, true},
		{"category changed", func(m *LiveMetadata) { m.Category = "Just Chatting" }, true},
		{"tag added", func(m *LiveMetadata) { m.Tags = []string{"a", "b"} }, true},
		{"extra added", func(m *LiveMetadata) { m.Extra = map[string]string{"k": "v"} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := base
			other.Tags = append([]string(nil), base.Tags...)
			tt.mod(&other)
			if got := base.Differs(other); got != tt.want {
				t.Errorf("Differs = %v, want %v", got, tt.want)
			}
		})
	}
}

// An empty slice and an absent one describe the same broadcast; a plugin that
// spells "no tags" the other way must not read as a change.
func TestLiveMetadataDiffersEmptyVsNilSlice(t *testing.T) {
	a := LiveMetadata{Title: "one", Tags: nil}
	b := LiveMetadata{Title: "one", Tags: []string{}}
	if a.Differs(b) {
		t.Error("nil tags and empty tags reported as a change")
	}
}

func TestLiveMetadataIsZero(t *testing.T) {
	if !(LiveMetadata{}).IsZero() {
		t.Error("empty metadata not reported as zero")
	}
	// A reading whose only content is a viewer count still says nothing about
	// the broadcast, and must not start a metadata sidecar on its own.
	if !(LiveMetadata{Viewers: 5}).IsZero() {
		t.Error("viewer-count-only metadata not reported as zero")
	}
	if (LiveMetadata{Title: "x"}).IsZero() {
		t.Error("metadata with a title reported as zero")
	}
}

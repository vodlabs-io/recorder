package streamer

import (
	"log/slog"
	"reflect"
)

// LiveMetadata is what the platform says about a broadcast, over and above the
// stream URLs needed to record it. Platforms describe a broadcast in wildly
// different terms, so the fields here are the ones that recur across several of
// them; anything one platform alone reports arrives in Extra.
//
// The json tags are load-bearing: this is decoded straight from cloud API
// payloads, which name the fields in snake_case. Go's fallback field matching is
// case-insensitive but not underscore-insensitive, so an untagged DisplayName
// never receives a "display_name" key.
type LiveMetadata struct {
	Platform       string `json:"platform,omitempty" log:"platform"`
	Title          string `json:"title,omitempty" log:"title"`
	DisplayName    string `json:"display_name,omitempty" log:"display_name"`
	PlatformUserID string `json:"platform_user_id,omitempty" log:"platform_user_id"`
	LiveType       string `json:"live_type,omitempty" log:"live_type"`
	// Category is what the broadcast is filed under: a game, a channel
	// category, a topic. Free text in the platform's own language.
	Category string `json:"category,omitempty" log:"category"`
	// Language is the broadcast language as the platform reports it — a BCP-47
	// tag where the platform uses one, otherwise its own code.
	Language string `json:"language,omitempty" log:"language"`
	// Tags are the streamer's free-form labels on the broadcast.
	Tags []string `json:"tags,omitempty"`
	// Viewers is the concurrent viewer count as of this reading, 0 when the
	// platform reports none. It is a sample rather than a property of the
	// broadcast: it moves between every check.
	Viewers int64 `json:"viewers,omitempty" log:"viewers"`
	// StartedAt is when the platform says the broadcast began, in Unix seconds.
	// It is the platform's own clock and can predate the recording by hours.
	StartedAt int64 `json:"started_at,omitempty" log:"started_at"`
	// ThumbnailURL is the platform's preview frame for the live broadcast. Most
	// platforms overwrite or expire it once the broadcast ends, so it is worth
	// archiving but not worth relying on.
	ThumbnailURL string `json:"thumbnail_url,omitempty" log:"thumbnail_url"`
	// Adult marks a broadcast the platform flags as mature or age-restricted.
	Adult bool `json:"adult,omitempty" log:"adult"`
	// Extra carries platform-specific readings that have no equivalent
	// anywhere else, keyed by the platform's own name for them.
	Extra map[string]string `json:"extra,omitempty"`
}

// IsZero reports whether the platform said nothing at all about the broadcast.
func (meta LiveMetadata) IsZero() bool {
	return !meta.Differs(LiveMetadata{})
}

// volatileFields are the LiveMetadata fields Differs ignores: readings that
// move on their own between two updates describing the same unchanged
// broadcast.
var volatileFields = map[string]bool{"Viewers": true}

// Differs reports whether o describes a different broadcast state than meta.
//
// The viewer count is exempt because it changes constantly, and a session that
// appended a reading every time it moved would archive thousands of them —
// stream URLs alone are re-resolved every few tens of seconds on some
// platforms, and each reply carries a fresh count.
//
// Fields are walked reflectively so a field added later is compared without
// anyone having to remember this function exists.
func (meta LiveMetadata) Differs(o LiveMetadata) bool {
	a, b := reflect.ValueOf(meta), reflect.ValueOf(o)
	for i := range a.NumField() {
		if volatileFields[a.Type().Field(i).Name] {
			continue
		}
		x, y := a.Field(i), b.Field(i)
		// An absent slice/map and an empty one say the same thing, and the
		// platforms spell it both ways; DeepEqual alone would call that a change.
		switch x.Kind() {
		case reflect.Slice, reflect.Map:
			if x.Len() == 0 && y.Len() == 0 {
				continue
			}
		}
		if !reflect.DeepEqual(x.Interface(), y.Interface()) {
			return true
		}
	}
	return false
}

// Attrs converts LiveMetadata to a slice of slog.Attr
func (meta *LiveMetadata) Attrs() []any {
	if meta == nil {
		return []any{}
	}

	attrs := []any{}

	v := reflect.ValueOf(*meta)
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		value := v.Field(i)

		// Get field name from struct tag, or use field name if tag not present
		fieldName := field.Tag.Get("log")
		if fieldName == "" {
			fieldName = field.Name
		}

		switch value.Kind() {
		case reflect.String:
			strValue := value.String()
			if strValue != "" {
				attrs = append(attrs, slog.String(fieldName, strValue))
			} else {
				attrs = append(attrs, slog.String(fieldName, "null"))
			}
		case reflect.Int64:
			// Numbers and flags are logged only when set: unlike the strings
			// above they have no "null", and a zero cannot be told apart from a
			// platform that never reports the field.
			if n := value.Int(); n != 0 {
				attrs = append(attrs, slog.Int64(fieldName, n))
			}
		case reflect.Bool:
			if value.Bool() {
				attrs = append(attrs, slog.Bool(fieldName, true))
			}
		}
	}

	return attrs
}

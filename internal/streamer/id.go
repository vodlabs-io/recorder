package streamer

import "strings"

// MaxSubscribed is headroom above the cloud's subscription cap, which is lower.
// Every per-streamer queue in the daemon is sized from it, so the largest burst
// (a subscription refresh, re-enabling recording for everyone) never blocks a
// producer.
const MaxSubscribed = 1000

// StreamerRef names a streamer before it is resolved: a StreamerKey or a URL.
type StreamerRef struct {
	StreamerKey
	// Links to the streamer's unique webpage on the platform
	URL string `json:"url,omitempty" toml:"url,omitempty"`
}

// Canonical returns the ID with its platform and username lowercased, matching
// the lowercase identities the cloud API returns, so streamers.toml, view-doc
// keys and cloud status stay aligned. The URL is left untouched (path case can
// be significant).
func (id StreamerRef) Canonical() StreamerRef {
	id.Platform = strings.ToLower(id.Platform)
	id.Username = strings.ToLower(id.Username)
	return id
}

func (s StreamerRef) String() string {
	if s.StreamerKey.Valid() {
		return s.StreamerKey.String()
	} else if s.URL != "" {
		return s.URL
	} else {
		return ""
	}
}

func (id StreamerRef) IsEmpty() bool {
	return id == StreamerRef{} || ((id.StreamerKey.Platform == "" || id.StreamerKey.Username == "") && id.URL == "")
}

type StreamerRefs []StreamerRef

func (ids StreamerRefs) Strings() []string {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	return strs
}

func (ids StreamerRefs) Streamers() List {
	streamers := make(List, len(ids))
	for i, id := range ids {
		streamers[i] = StreamerState{
			StreamerRef: id,
			Status:      StatusUnknown,
		}
	}
	return streamers
}

func (ids StreamerRefs) Keys() StreamerKeys {
	keys := make(StreamerKeys, len(ids))
	for i, id := range ids {
		keys[i] = id.StreamerKey
	}
	return keys
}

// Contains reports whether ids includes id, compared by String() (so a
// platform/username ID matches regardless of an unset URL, and vice versa).
func (ids StreamerRefs) Contains(id StreamerRef) bool {
	target := id.String()
	if target == "" {
		return false
	}
	for _, e := range ids {
		if e.String() == target {
			return true
		}
	}
	return false
}

func NewIDs(strs []string) StreamerRefs {
	ids := make(StreamerRefs, len(strs))
	for i, str := range strs {
		ids[i] = StreamerRef{URL: str}
	}
	return ids
}

func (oldIDs StreamerRefs) Diff(newIDs StreamerRefs) (added, removed StreamerRefs) {
	oldMap := make(map[string]StreamerRef)
	newMap := make(map[string]StreamerRef)

	for _, id := range oldIDs {
		if id.IsEmpty() {
			continue
		}
		oldMap[id.String()] = id
	}
	for _, id := range newIDs {
		if id.IsEmpty() {
			continue
		}
		newMap[id.String()] = id
	}

	for str, id := range newMap {
		if _, exists := oldMap[str]; !exists {
			added = append(added, id)
		}
	}
	for str, id := range oldMap {
		if _, exists := newMap[str]; !exists {
			removed = append(removed, id)
		}
	}

	return added, removed
}

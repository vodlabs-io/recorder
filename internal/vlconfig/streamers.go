package vlconfig

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	tomlv2 "github.com/pelletier/go-toml/v2"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// StreamersFile is the full parsed contents of streamers.toml.
type StreamersFile struct {
	path            string
	mu              sync.RWMutex
	lastWrittenHash [32]byte
	// listed holds the IDs from the [streamers] section verbatim.
	listed streamer.StreamerRefs
	// rules preserved verbatim from the [[rules]] section.
	rules []streamerRule
}

func NewStreamersFile(path string) (*StreamersFile, error) {
	sf := &StreamersFile{path: path}
	if err := sf.Load(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(sf.path)
	if err != nil {
		return nil, err
	}
	sf.lastWrittenHash = sha256.Sum256(data)
	return sf, nil
}

func (sf *StreamersFile) changed() (bool, error) {
	sf.mu.RLock()
	defer sf.mu.RUnlock()

	data, err := os.ReadFile(sf.path)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(data)
	return !bytes.Equal(sf.lastWrittenHash[:], hash[:]), err
}

// CurrentHash returns the SHA-256 of the file's current on-disk contents.
func (sf *StreamersFile) CurrentHash() ([32]byte, error) {
	data, err := os.ReadFile(sf.path)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// LastHash returns the hash of the contents the daemon itself last wrote. Used
// to distinguish external edits from the daemon's own writes.
func (sf *StreamersFile) LastHash() [32]byte {
	sf.mu.RLock()
	defer sf.mu.RUnlock()
	return sf.lastWrittenHash
}

// AllIDs returns the de-duplicated union of Listed IDs and IDs covered by
// rules. This is the set of streamers the daemon should subscribe to.
func (sf *StreamersFile) AllIDs() streamer.StreamerRefs {
	sf.mu.RLock()
	defer sf.mu.RUnlock()

	seen := make(map[string]struct{}, len(sf.listed))
	out := make(streamer.StreamerRefs, 0, len(sf.listed))
	add := func(id streamer.StreamerRef) {
		s := id.String()
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, id)
	}
	for _, id := range sf.listed {
		add(id)
	}
	for _, r := range sf.rules {
		for _, id := range r.ids() {
			add(id)
		}
	}
	return out
}

// Load parses streamers.toml in full.
func (sf *StreamersFile) Load() error {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	if err := EnsureStreamersFileExists(sf.path); err != nil {
		return fmt.Errorf("failed to ensure streamers file exists: %w", err)
	}

	k := koanf.New(".")
	if err := k.Load(file.Provider(sf.path), toml.Parser()); err != nil {
		return fmt.Errorf("failed to load toml file: %w", err)
	}

	listed, err := loadListed(k)
	if err != nil {
		return err
	}
	sf.listed = listed

	rules, err := loadRules(k)
	if err != nil {
		return err
	}
	sf.rules = rules
	return nil
}

// urlsKey is the one key in a [streamers] or [[rules]] table that holds URLs
// rather than a platform's usernames.
const urlsKey = "urls"

// stringSliceValue reports whether a decoded TOML value is an array of strings —
// the shape of a platform's username list — and returns its contents.
//
// This tells a platform key apart from anything else in the table without
// consulting the supported-platform list. Options are scalars, so an unknown
// option is not mistaken for a platform.
func stringSliceValue(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// tableKeys returns the keys of a decoded TOML table in sorted order. Sorting
// makes parsing deterministic; the written order is decided separately by
// platformKeyOrder.
func tableKeys(v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}

// platformKeyOrder orders the platform keys of a table for writing: the
// platforms this build knows about first, in PlatformNames order so the file
// keeps its familiar shape, then everything else alphabetically. Empty lists are
// omitted.
func platformKeyOrder(byPlatform map[string][]string) []string {
	names := PlatformNames()
	out := make([]string, 0, len(byPlatform))
	for _, p := range names {
		if len(byPlatform[p]) > 0 {
			out = append(out, p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(byPlatform)) {
		if len(byPlatform[p]) > 0 && !slices.Contains(names, p) {
			out = append(out, p)
		}
	}
	return out
}

// loadListed reads the [streamers] table.
//
// Every key that holds an array of strings is taken as a platform's username
// list, whether or not the supported-platform list names that platform. That
// list comes from the cloud and can be short or stale, and Write serializes the
// whole model, so filtering by it would erase user entries.
func loadListed(k *koanf.Koanf) (streamer.StreamerRefs, error) {
	var strCfg streamersConfig
	if err := k.Unmarshal("streamers", &strCfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal streamers config: %w", err)
	}
	listed := streamer.NewIDs(strCfg.URLs)
	for _, platform := range tableKeys(k.Get("streamers")) {
		if platform == urlsKey {
			continue
		}
		usernames, ok := stringSliceValue(k.Get("streamers." + platform))
		if !ok {
			continue // not a username list; nothing here names a streamer
		}
		for _, username := range usernames {
			if username == "" {
				continue
			}
			// Canonicalize on the way in: every other identity in the daemon
			// (cloud view docs, KV keys, vlapi requests) is lowercase, so a
			// mixed-case entry (e.g. hand-edited) would never match one.
			// Folding here means the in-memory model is uniformly canonical
			// and the next Write() heals the file.
			listed = append(listed, streamer.StreamerRef{
				StreamerKey: streamer.StreamerKey{Platform: platform, Username: username},
			}.Canonical())
		}
	}
	return listed, nil
}

func loadRules(k *koanf.Koanf) ([]streamerRule, error) {
	var rules []streamerRule
	for _, rk := range k.Slices("rules") {
		rule, err := parseRule(rk)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// parseRule reads one [[rules]] table. Like loadListed it keys off the table's
// own contents rather than the supported-platform list, so a rule for an
// unknown platform survives. A key is a platform's username list when it is
// neither urls nor an option name, and its value is an array of strings.
func parseRule(rk *koanf.Koanf) (streamerRule, error) {
	rule := streamerRule{usernames: map[string][]string{}}
	options := ruleOptionKeys()
	for _, p := range tableKeys(rk.Raw()) {
		if p == urlsKey || slices.Contains(options, p) {
			continue
		}
		users, ok := stringSliceValue(rk.Get(p))
		if !ok || len(users) == 0 {
			continue
		}
		// Same canonicalization as loadListed
		for i, u := range users {
			users[i] = strings.ToLower(u)
		}
		rule.usernames[p] = users
	}
	if err := rk.Unmarshal(urlsKey, &rule.uRLs); err != nil {
		return rule, fmt.Errorf("failed to unmarshal rule urls: %w", err)
	}
	rule.RuleOptions.readFrom(rk)
	return rule, nil
}

// tomlKV is a single key/value pair for ordered TOML emission. go-toml/v2
// always sorts map keys alphabetically, so to control the on-disk key order we
// drive it ourselves through an ordered slice of these rather than a map.
type tomlKV struct {
	key string
	val any
}

// Write serializes a StreamersFile to disk. Within [streamers] and each
// [[rules]] entry the keys are emitted in a deliberate order: options before
// username lists, and urls before username lists, rather than the
// alphabetical order a map would impose, keeping the file easy to read.
func (sf *StreamersFile) Write() error {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	// Normalize the rule set before serializing so rules with identical options
	// collapse into one entry, regardless of how they were produced.
	sf.rules = consolidateRules(sf.rules)

	var buf bytes.Buffer
	buf.WriteString("[streamers]\n")
	if err := writeKVs(&buf, listedToKVs(sf.listed)); err != nil {
		return fmt.Errorf("failed to encode streamers: %w", err)
	}
	for _, r := range sf.rules {
		buf.WriteString("\n[[rules]]\n")
		if err := writeKVs(&buf, r.toKVs()); err != nil {
			return fmt.Errorf("failed to encode rule: %w", err)
		}
	}

	if err := writeFileAtomic(sf.path, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write streamers file %s: %w", sf.path, err)
	}
	sf.lastWrittenHash = sha256.Sum256(buf.Bytes())
	return nil
}

// writeKVs writes each pair to buf as a TOML line, preserving slice order.
// Value formatting (string escaping, multiline arrays) is delegated to
// go-toml/v2 by encoding a single-key map per pair.
func writeKVs(buf *bytes.Buffer, kvs []tomlKV) error {
	for _, kv := range kvs {
		var line bytes.Buffer
		enc := tomlv2.NewEncoder(&line)
		enc.SetArraysMultiline(true)
		if err := enc.Encode(map[string]any{kv.key: kv.val}); err != nil {
			return err
		}
		buf.Write(line.Bytes())
	}
	return nil
}

// listedToKVs encodes the [streamers] table as ordered pairs: urls first, then
// platform username lists in platformKeyOrder. urls is always emitted (even when
// empty) to keep the section's shape stable across writes.
//
// Every entry is written, including one whose platform the supported-platform
// list does not name (see loadListed).
func listedToKVs(ids streamer.StreamerRefs) []tomlKV {
	byPlatform := map[string][]string{}
	urls := []string{}
	for _, id := range ids {
		switch {
		case id.StreamerKey.Valid():
			byPlatform[id.StreamerKey.Platform] = append(byPlatform[id.StreamerKey.Platform], id.StreamerKey.Username)
		case id.URL != "":
			urls = append(urls, id.URL)
		}
	}
	kvs := []tomlKV{{urlsKey, urls}}
	for _, p := range platformKeyOrder(byPlatform) {
		kvs = append(kvs, tomlKV{p, byPlatform[p]})
	}
	return kvs
}

// AddID lists id in the [streamers] section. It is idempotent: an id already
// listed is left as-is, so callers can add a streamer without first checking
// membership and never produce a duplicate entry.
func (sf *StreamersFile) AddID(id streamer.StreamerRef) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	target := id.String()
	for _, e := range sf.listed {
		if e.String() == target {
			return
		}
	}
	sf.listed = append(sf.listed, id)
}

// DropURLs removes URL entries from [streamers] and from every rule, returning
// the ones it actually found. Callers write the file afterwards.
//
// This is how a URL entry that names no streamer (e.g. hand-written) leaves the
// config. Matching is by exact URL string, like Normalize. Only URL entries are
// eligible, unlike RemoveID: a urls entry reading "tiktok.alice" stringifies
// like the real tiktok/alice.
func (sf *StreamersFile) DropURLs(urls []string) []string {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	drop := make(map[string]struct{}, len(urls))
	for _, u := range urls {
		if u != "" {
			drop[u] = struct{}{}
		}
	}
	if len(drop) == 0 {
		return nil
	}
	found := make(map[string]struct{}, len(drop))
	doomed := func(id streamer.StreamerRef) bool {
		if id.URL == "" || id.StreamerKey.Valid() {
			return false
		}
		if _, ok := drop[id.URL]; !ok {
			return false
		}
		found[id.URL] = struct{}{}
		return true
	}

	sf.listed = slices.DeleteFunc(sf.listed, doomed)

	newRules := make([]streamerRule, 0, len(sf.rules))
	for _, r := range sf.rules {
		r.uRLs = slices.DeleteFunc(r.uRLs, func(u string) bool {
			return doomed(streamer.StreamerRef{URL: u})
		})
		if r.isEmpty() {
			continue // its only members were the entries just dropped
		}
		newRules = append(newRules, r)
	}
	sf.rules = newRules

	// In the order asked, so the caller's log reads in the order the cloud
	// reported the rejections.
	dropped := make([]string, 0, len(found))
	for _, u := range urls {
		if _, ok := found[u]; ok {
			delete(found, u)
			dropped = append(dropped, u)
		}
	}
	return dropped
}

// RemoveID removes the given streamer from [streamers] and from any matching
// rule. Empty rules are dropped. Returns true if anything was removed.
func (sf *StreamersFile) RemoveID(id streamer.StreamerRef) bool {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	removed := false
	// Compare canonically, as streamerRule.removeID below does.
	target := id.Canonical().String()

	filtered := make(streamer.StreamerRefs, 0, len(sf.listed))
	for _, e := range sf.listed {
		if e.Canonical().String() == target {
			removed = true
			continue
		}
		filtered = append(filtered, e)
	}
	sf.listed = filtered

	newRules := make([]streamerRule, 0, len(sf.rules))
	for _, r := range sf.rules {
		if r.removeID(id) {
			removed = true
		}
		if r.isEmpty() {
			continue
		}
		newRules = append(newRules, r)
	}
	sf.rules = newRules
	return removed
}

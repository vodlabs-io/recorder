package vlconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// writeFile writes the given content to a fresh streamers.toml under t.TempDir()
// and returns the absolute path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "streamers.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// loadFile writes content and returns a loaded StreamersFile.
func loadFile(t *testing.T, content string) *StreamersFile {
	t.Helper()
	path := writeFile(t, content)
	sf, err := NewStreamersFile(path)
	if err != nil {
		t.Fatalf("NewStreamersFile: %v", err)
	}
	return sf
}

func keyID(platform, username string) streamer.StreamerRef {
	return streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: platform, Username: username}}
}

func urlID(u string) streamer.StreamerRef {
	return streamer.StreamerRef{URL: u}
}

// idStrings returns id.String() for each id, sorted. Useful for set comparison.
func idStrings(ids streamer.StreamerRefs) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

func TestLoad_PlatformAndURLs(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob"]
urls = ["https://example.com/a"]
`)
	got := idStrings(sf.AllIDs())
	want := []string{"https://example.com/a", "tiktok.alice", "tiktok.bob"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllIDs = %v, want %v", got, want)
	}
}

func TestLoad_Rules(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
monitor = false
record = false
`)
	if len(sf.rules) != 1 {
		t.Fatalf("len(Rules) = %d, want 1", len(sf.rules))
	}
	r := sf.rules[0]
	if r.Monitor == nil || *r.Monitor != false {
		t.Errorf("Monitor = %v, want non-nil false", r.Monitor)
	}
	if r.Record == nil || *r.Record != false {
		t.Errorf("Record = %v, want non-nil false", r.Record)
	}
	if !slices.Equal(r.usernames["tiktok"], []string{"alice"}) {
		t.Errorf("Usernames[tiktok] = %v, want [alice]", r.usernames["tiktok"])
	}
}

func TestRoundTrip_FullShape(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob"]
pandalive = ["carol"]
urls = ["https://example.com/x"]

[[rules]]
tiktok = ["alice"]
monitor = false

[[rules]]
pandalive = ["carol"]
record = false
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}

	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if !reflect.DeepEqual(idStrings(sf.AllIDs()), idStrings(sf2.AllIDs())) {
		t.Errorf("AllIDs differs after round-trip:\n got %v\nwant %v", idStrings(sf2.AllIDs()), idStrings(sf.AllIDs()))
	}
	if len(sf2.rules) != 2 {
		t.Fatalf("Rules count after round-trip = %d, want 2", len(sf2.rules))
	}
	// Verify both rule flags survive.
	var sawMonitor, sawRecordOff bool
	for _, r := range sf2.rules {
		if r.Monitor != nil && !*r.Monitor {
			sawMonitor = true
		}
		if r.Record != nil && !*r.Record {
			sawRecordOff = true
		}
	}
	if !sawMonitor || !sawRecordOff {
		t.Errorf("flags after round-trip: sawMonitor=%v sawRecordOff=%v", sawMonitor, sawRecordOff)
	}
}

func TestOptionsFor(t *testing.T) {
	alice := keyID("tiktok", "alice")
	bob := keyID("tiktok", "bob")

	tests := []struct {
		name   string
		toml   string
		id     streamer.StreamerRef
		expect streamerOptions
	}{
		{
			name:   "no rule → default Monitor=true",
			toml:   `[streamers]` + "\n" + `tiktok = ["alice"]`,
			id:     alice,
			expect: streamerOptions{Monitor: true, Record: true},
		},
		{
			name: "monitor=false rule applies",
			toml: `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
monitor = false
`,
			id:     alice,
			expect: streamerOptions{Monitor: false, Record: true},
		},
		{
			name: "record=false rule applies",
			toml: `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
record = false
`,
			id:     alice,
			expect: streamerOptions{Monitor: true, Record: false},
		},
		{
			name: "later rule overrides earlier",
			toml: `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
monitor = false

[[rules]]
tiktok = ["alice"]
monitor = true
`,
			id:     alice,
			expect: streamerOptions{Monitor: true, Record: true},
		},
		{
			name: "variant rule overrides default",
			toml: `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
variant = "720p"
`,
			id:     alice,
			expect: streamerOptions{Monitor: true, Record: true, Variant: "720p"},
		},
		{
			name: "rule doesn't match a different id",
			toml: `[streamers]
tiktok = ["alice", "bob"]

[[rules]]
tiktok = ["alice"]
record = false
`,
			id:     bob,
			expect: streamerOptions{Monitor: true, Record: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sf := loadFile(t, tc.toml)
			got := sf.OptionsFor(tc.id)
			if got != tc.expect {
				t.Errorf("OptionsFor(%v) = %+v, want %+v", tc.id, got, tc.expect)
			}
		})
	}
}

func TestAddOneShotRule_AndHas(t *testing.T) {
	sf := loadFile(t, `[streamers]`)
	id := keyID("tiktok", "alice")

	if sf.HasOneShotRule(id) {
		t.Fatalf("HasOneShotRule before add = true, want false")
	}
	if err := sf.AddOneShotRule(id); err != nil {
		t.Fatalf("AddOneShotRule: %v", err)
	}
	if !sf.HasOneShotRule(id) {
		t.Errorf("HasOneShotRule after add = false, want true")
	}
	if err := sf.AddOneShotRule(id); err == nil {
		t.Errorf("AddOneShotRule duplicate: err = nil, want non-nil")
	}
}

func TestSetVariantRule(t *testing.T) {
	sf := loadFile(t, `[streamers]`)
	id := keyID("tiktok", "alice")

	if got := sf.OptionsFor(id).Variant; got != "" {
		t.Fatalf("Variant before set = %q, want empty", got)
	}

	// Set initial variant.
	if err := sf.SetVariantRule(id, "720p"); err != nil {
		t.Fatalf("SetVariantRule: %v", err)
	}
	if got := sf.OptionsFor(id).Variant; got != "720p" {
		t.Errorf("Variant after set = %q, want 720p", got)
	}

	// Setting again replaces the prior rule (no stacking).
	if err := sf.SetVariantRule(id, "best"); err != nil {
		t.Fatalf("SetVariantRule second: %v", err)
	}
	if got := sf.OptionsFor(id).Variant; got != "best" {
		t.Errorf("Variant after replace = %q, want best", got)
	}
	if len(sf.rules) != 1 {
		t.Errorf("Rules after replace = %d, want 1 (old rule removed)", len(sf.rules))
	}

	// Empty variant clears.
	if err := sf.SetVariantRule(id, ""); err != nil {
		t.Fatalf("SetVariantRule clear: %v", err)
	}
	if got := sf.OptionsFor(id).Variant; got != "" {
		t.Errorf("Variant after clear = %q, want empty", got)
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after clear = %d, want 0", len(sf.rules))
	}
}

// Clearing variant on one entry of a multi-entry rule leaves siblings intact.
func TestRemoveVariantRule_MultiEntry_OnlyClearsOne(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice", "bob"]
variant = "720p"
`)
	alice := keyID("tiktok", "alice")
	bob := keyID("tiktok", "bob")

	if !sf.RemoveVariantRule(alice) {
		t.Fatalf("RemoveVariantRule = false, want true")
	}
	if got := sf.OptionsFor(alice).Variant; got != "" {
		t.Errorf("Variant(alice) after clear = %q, want empty", got)
	}
	if got := sf.OptionsFor(bob).Variant; got != "720p" {
		t.Errorf("Variant(bob) after clear = %q, want 720p (sibling stays)", got)
	}
}

func TestAddRecordOffRule_AndIsRecording(t *testing.T) {
	sf := loadFile(t, `[streamers]`)
	id := keyID("tiktok", "alice")

	if !sf.IsRecording(id) {
		t.Fatalf("IsRecording before add = false, want true")
	}
	if err := sf.AddRecordOffRule(id); err != nil {
		t.Fatalf("AddRecordOffRule: %v", err)
	}
	if sf.IsRecording(id) {
		t.Errorf("IsRecording after add = true, want false")
	}
	if !sf.RemoveRecordOffRule(id) {
		t.Errorf("RemoveRecordOffRule = false, want true")
	}
	if !sf.IsRecording(id) {
		t.Errorf("IsRecording after remove = false, want true")
	}
}

func TestRemoveDisableRule_SoloEntry(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice"]
record = false
`)
	if !sf.RemoveRecordOffRule(keyID("tiktok", "alice")) {
		t.Fatalf("RemoveDisableRule = false, want true")
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after remove = %d, want 0", len(sf.rules))
	}
}

// Clearing record=false for one entry of a multi-entry rule switches only that
// entry back on; siblings stay off.
func TestRemoveDisableRule_MultiEntry_OnlyClearsOne(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice", "bob"]
record = false
`)
	alice := keyID("tiktok", "alice")
	bob := keyID("tiktok", "bob")

	if sf.IsRecording(bob) {
		t.Fatalf("IsRecording(bob) before = true, want false")
	}
	if !sf.RemoveRecordOffRule(alice) {
		t.Fatalf("RemoveRecordOffRule = false, want true")
	}
	if !sf.IsRecording(alice) {
		t.Errorf("IsRecording(alice) after = false, want true")
	}
	if sf.IsRecording(bob) {
		t.Errorf("IsRecording(bob) after = true, want false (sibling stays switched off)")
	}
	if len(sf.rules) != 1 {
		t.Fatalf("Rules count = %d, want 1", len(sf.rules))
	}
	if !slices.Equal(sf.rules[0].usernames["tiktok"], []string{"bob"}) {
		t.Errorf("remaining Usernames = %v, want [bob]", sf.rules[0].usernames["tiktok"])
	}
	if sf.rules[0].Record == nil || *sf.rules[0].Record {
		t.Errorf("Record on remaining rule = %v, want non-nil false", sf.rules[0].Record)
	}
}

func TestRemoveDisableRule_NoMatch(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["bob"]
record = false
`)
	if sf.RemoveRecordOffRule(keyID("tiktok", "alice")) {
		t.Errorf("RemoveDisableRule = true, want false")
	}
	if len(sf.rules) != 1 {
		t.Errorf("Rules count = %d, want 1", len(sf.rules))
	}
}

func TestRemoveID(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob"]

[[rules]]
tiktok = ["alice"]
monitor = false
`)
	alice := keyID("tiktok", "alice")

	if !sf.RemoveID(alice) {
		t.Fatalf("RemoveID = false, want true")
	}
	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs after remove = %v, want %v", got, want)
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after remove = %d, want 0 (empty rule dropped)", len(sf.rules))
	}
	if sf.RemoveID(alice) {
		t.Errorf("RemoveID second call = true, want false")
	}
}

// TestRemoveID_CaseInsensitive covers a streamer added with an uppercase
// username: the UI removes it by the lowercase form, so RemoveID must match
// case-insensitively.
func TestRemoveID_CaseInsensitive(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["Examplename", "bob"]
`)
	if !sf.RemoveID(keyID("tiktok", "examplename")) {
		t.Fatalf("RemoveID with lowercase = false, want true")
	}
	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs after remove = %v, want %v", got, want)
	}
}

// TestRemoveID_CaseInsensitive_RuleOnly covers an uppercase username that
// exists only in a [[rules]] entry. AllIDs unions the listed and rule-covered
// IDs, so a rule entry the lowercase remove can't match would keep the
// streamer subscribed.
func TestRemoveID_CaseInsensitive_RuleOnly(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["bob"]

[[rules]]
tiktok = ["Examplename", "bob"]
`)
	if !sf.RemoveID(keyID("tiktok", "examplename")) {
		t.Fatalf("RemoveID with lowercase = false, want true")
	}
	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs after remove = %v, want %v", got, want)
	}
}

// TestLoad_CanonicalizesUsernames pins the fold to parse time, so every later
// lookup (matches → OptionsFor/IsRecording, AllIDs, the KV doc key) keys on the
// same identity the cloud and the UI use.
func TestLoad_CanonicalizesUsernames(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["Examplename"]

[[rules]]
tiktok = ["EXAMPLENAME"]
monitor = false
`)
	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.examplename"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs = %v, want %v", got, want)
	}
	if opts := sf.OptionsFor(keyID("tiktok", "examplename")); opts.Monitor {
		t.Errorf("OptionsFor(...).Monitor = true, want false (mixed-case rule must apply)")
	}
}

func TestRemoveID_URL(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://x.com/a", "https://x.com/b"]
`)
	if !sf.RemoveID(urlID("https://x.com/a")) {
		t.Fatalf("RemoveID URL = false, want true")
	}
	got := idStrings(sf.AllIDs())
	want := []string{"https://x.com/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs after remove = %v, want %v", got, want)
	}
}

func TestPurgeOneShots(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob"]

[[rules]]
tiktok = ["alice"]
monitor = false
`)
	removed := sf.PurgeOneShots()
	if len(removed) != 1 || removed[0].ID.String() != "tiktok.alice" {
		t.Errorf("removed = %+v, want [tiktok.alice]", removed)
	}
	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.bob"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs after purge = %v, want %v", got, want)
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after purge = %d, want 0", len(sf.rules))
	}
}

func TestPurgeOneShots_CapturesOptions(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice"]
monitor = false
variant = "720p"
output_template = "{username}/{date}"
delete_part_files = true
split_size = "2GB"
split_duration = "1h30m"
`)
	removed := sf.PurgeOneShots()
	if len(removed) != 1 || removed[0].ID.String() != "tiktok.alice" {
		t.Fatalf("removed = %+v, want [tiktok.alice]", removed)
	}
	opts := removed[0].Opts
	if opts.Variant != "720p" {
		t.Errorf("Opts.Variant = %q, want %q", opts.Variant, "720p")
	}
	if opts.OutputTemplate != "{username}/{date}" {
		t.Errorf("Opts.OutputTemplate = %q, want %q", opts.OutputTemplate, "{username}/{date}")
	}
	if opts.DeletePartFiles == nil || !*opts.DeletePartFiles {
		t.Errorf("Opts.DeletePartFiles = %v, want true", opts.DeletePartFiles)
	}
	// Split overrides must be captured so they survive the purge into KV.
	if opts.SplitSize != "2GB" {
		t.Errorf("Opts.SplitSize = %q, want 2GB", opts.SplitSize)
	}
	if opts.SplitDuration != "1h30m" {
		t.Errorf("Opts.SplitDuration = %q, want 1h30m", opts.SplitDuration)
	}
}

func TestPurgeOneShots_URLDroppedSilently(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://x.com/a"]

[[rules]]
urls = ["https://x.com/a"]
monitor = false
`)
	removed := sf.PurgeOneShots()
	if len(removed) != 0 {
		t.Errorf("removed = %+v, want [] (URL entries are dropped silently)", removed)
	}
	if len(sf.AllIDs()) != 0 {
		t.Errorf("AllIDs after purge = %v, want empty", idStrings(sf.AllIDs()))
	}
}

func TestPurgeOneShots_LeavesNonOneShot(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
record = false
`)
	removed := sf.PurgeOneShots()
	if len(removed) != 0 {
		t.Errorf("removed = %+v, want []", removed)
	}
	if len(sf.rules) != 1 {
		t.Errorf("Rules after purge = %d, want 1", len(sf.rules))
	}
}

func TestNormalize(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://example.com/alice"]

[[rules]]
urls = ["https://example.com/alice"]
monitor = false
`)
	resolved := streamer.StreamerRefs{{
		StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "alice"},
		URL:         "https://example.com/alice",
	}}
	sf.Normalize(resolved)

	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs after Normalize = %v, want %v", got, want)
	}
	if len(sf.rules) != 1 {
		t.Fatalf("Rules after Normalize = %d, want 1", len(sf.rules))
	}
	r := sf.rules[0]
	if !slices.Equal(r.usernames["tiktok"], []string{"alice"}) {
		t.Errorf("rule Usernames = %v, want [alice]", r.usernames["tiktok"])
	}
	if len(r.uRLs) != 0 {
		t.Errorf("rule URLs = %v, want empty", r.uRLs)
	}
}

func TestNormalize_UnresolvedURLStays(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://example.com/unknown"]
`)
	sf.Normalize(streamer.StreamerRefs{}) // empty resolution map

	got := idStrings(sf.AllIDs())
	want := []string{"https://example.com/unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs = %v, want %v", got, want)
	}
}

func TestChanged_AfterConstruct(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]
`)
	changed, err := sf.changed()
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if changed {
		t.Errorf("Changed immediately after NewStreamersFile = true, want false")
	}
}

func TestChanged_AfterWrite(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	changed, err := sf.changed()
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if changed {
		t.Errorf("Changed immediately after Write = true, want false")
	}

	// External modification → Changed returns true.
	data, _ := os.ReadFile(sf.path)
	if err := os.WriteFile(sf.path, append(data, []byte("\n# touched\n")...), 0644); err != nil {
		t.Fatalf("touch file: %v", err)
	}
	changed, err = sf.changed()
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if !changed {
		t.Errorf("Changed after external touch = false, want true")
	}
}

func TestAllIDs_Dedup(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
record = false
`)
	got := idStrings(sf.AllIDs())
	want := []string{"tiktok.alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs = %v, want %v", got, want)
	}
}

func TestWrite_PreservesStreamersSection(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]
urls = ["https://example.com/a"]
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := os.ReadFile(sf.path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "alice") {
		t.Errorf("written file missing 'alice':\n%s", s)
	}
	if !strings.Contains(s, "https://example.com/a") {
		t.Errorf("written file missing URL:\n%s", s)
	}
}

// TestWrite_KeyOrdering verifies the deliberate on-disk key order: in
// [streamers], urls precedes username lists; in [[rules]], options precede
// username lists. Input is given in the "wrong" order to prove Write reorders.
func TestWrite_KeyOrdering(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]
urls = ["https://example.com/x"]

[[rules]]
tiktok = ["bob"]
record = false
monitor = false
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := os.ReadFile(sf.path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	s := string(out)

	// before asserts that needle appears before later in s.
	before := func(needle, later string) {
		t.Helper()
		i, j := strings.Index(s, needle), strings.Index(s, later)
		if i < 0 || j < 0 {
			t.Fatalf("missing %q (%d) or %q (%d) in:\n%s", needle, i, later, j, s)
		}
		if i > j {
			t.Errorf("expected %q before %q in:\n%s", needle, later, s)
		}
	}

	// [streamers]: urls above the username list.
	before("urls = [", "tiktok = [")
	// [[rules]]: options above the username list.
	before("monitor = false", "tiktok = [\n  'bob'")
	before("record = false", "tiktok = [\n  'bob'")
}

// TestWrite_ConsolidatesRules verifies that Write merges rules carrying
// identical options into one, while leaving a rule with different options
// separate.
func TestWrite_ConsolidatesRules(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
disabled = false
variant = "worst"
tiktok = ["alice", "bob"]

[[rules]]
disabled = false
tiktok = ["carol", "fred", "amy"]

[[rules]]
disabled = false
tiktok = ["josh", "lisa"]
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if len(sf2.rules) != 2 {
		t.Fatalf("Rules after consolidation = %d, want 2", len(sf2.rules))
	}

	// The variant='worst' rule keeps its two streamers; the plain rule absorbs
	// the five from the two mergeable rules.
	var variantRule, plainRule *streamerRule
	for i := range sf2.rules {
		if sf2.rules[i].Variant != nil {
			variantRule = &sf2.rules[i]
		} else {
			plainRule = &sf2.rules[i]
		}
	}
	if variantRule == nil || plainRule == nil {
		t.Fatalf("expected one variant rule and one plain rule, got %+v", sf2.rules)
	}
	if got := len(variantRule.usernames["tiktok"]); got != 2 {
		t.Errorf("variant rule covers %d streamers, want 2", got)
	}
	if got := len(plainRule.usernames["tiktok"]); got != 5 {
		t.Errorf("plain rule covers %d streamers, want 5", got)
	}
}

// A streamer present in two mergeable rules is listed only once after merge.
func TestWrite_ConsolidatesRules_DedupesStreamer(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
record = false
tiktok = ["alice"]

[[rules]]
record = false
tiktok = ["alice", "bob"]
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if len(sf2.rules) != 1 {
		t.Fatalf("Rules = %d, want 1", len(sf2.rules))
	}
	got := sf2.rules[0].usernames["tiktok"]
	if len(got) != 2 {
		t.Errorf("merged rule Usernames = %v, want [alice bob] (deduped)", got)
	}
}

func TestDisableAll_AddsSingleRule(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob", "carol"]

[[rules]]
tiktok = ["bob"]
record = false
`)
	if !sf.RecordOffAll() {
		t.Fatalf("RecordOffAll = false, want true")
	}
	// alice and carol get switched off; bob already was. One new rule covers the
	// two newly switched-off streamers.
	for _, u := range []string{"alice", "bob", "carol"} {
		if sf.IsRecording(keyID("tiktok", u)) {
			t.Errorf("IsRecording(%s) = true, want false", u)
		}
	}
	// A second RecordOffAll is a no-op now everyone is switched off.
	if sf.RecordOffAll() {
		t.Errorf("second DisableAll = true, want false (all already disabled)")
	}
}

func TestEnableAll_ClearsDisabled(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob"]

[[rules]]
tiktok = ["alice", "bob"]
record = false

[[rules]]
tiktok = ["alice"]
variant = "worst"
`)
	if !sf.RecordOnAll() {
		t.Fatalf("EnableAll = false, want true")
	}
	if !sf.IsRecording(keyID("tiktok", "alice")) || !sf.IsRecording(keyID("tiktok", "bob")) {
		t.Errorf("streamers still disabled after EnableAll")
	}
	// The pure disabled rule is dropped; the variant rule survives intact.
	if len(sf.rules) != 1 {
		t.Fatalf("Rules after EnableAll = %d, want 1 (variant rule kept)", len(sf.rules))
	}
	if sf.OptionsFor(keyID("tiktok", "alice")).Variant != "worst" {
		t.Errorf("variant lost after EnableAll")
	}
	if sf.RecordOnAll() {
		t.Errorf("second EnableAll = true, want false (nothing disabled)")
	}
}

// Streamers that live only inside a record=false rule (nothing in [streamers])
// must survive RecordOnAll: dropping the rule re-homes them into [streamers].
func TestEnableAll_PreservesRuleOnlyStreamers(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = []

[[rules]]
record = false
tiktok = ["user1", "user2", "user3"]
`)
	if !sf.RecordOnAll() {
		t.Fatalf("EnableAll = false, want true")
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after EnableAll = %d, want 0 (pure disabled rule dropped)", len(sf.rules))
	}
	want := []string{"tiktok.user1", "tiktok.user2", "tiktok.user3"}
	if got := idStrings(sf.AllIDs()); !slices.Equal(got, want) {
		t.Errorf("AllIDs after EnableAll = %v, want %v (streamers preserved)", got, want)
	}
	for _, u := range []string{"user1", "user2", "user3"} {
		if !sf.IsRecording(keyID("tiktok", u)) {
			t.Errorf("IsRecording(%s) = true, want false (enabled)", u)
		}
	}
}

// A streamer that exists only inside a record=false rule must survive a
// per-row record_on too: it is re-homed into [streamers].
func TestRemoveDisableRule_PreservesRuleOnlyStreamer(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
record = false
tiktok = ["alice"]
`)
	alice := keyID("tiktok", "alice")
	if !sf.RemoveRecordOffRule(alice) {
		t.Fatalf("RemoveDisableRule = false, want true")
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after remove = %d, want 0", len(sf.rules))
	}
	if got := idStrings(sf.AllIDs()); !slices.Equal(got, []string{"tiktok.alice"}) {
		t.Errorf("AllIDs after remove = %v, want [tiktok.alice] (preserved)", got)
	}
	if !sf.IsRecording(alice) {
		t.Errorf("IsRecording(alice) = true, want false")
	}
}

func TestRemoveAll_ClearsEverything(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice", "bob"]

[[rules]]
tiktok = ["alice"]
record = false
`)
	if !sf.RemoveAll() {
		t.Fatalf("RemoveAll = false, want true")
	}
	if len(sf.AllIDs()) != 0 {
		t.Errorf("AllIDs after RemoveAll = %v, want empty", idStrings(sf.AllIDs()))
	}
	if len(sf.rules) != 0 {
		t.Errorf("Rules after RemoveAll = %d, want 0", len(sf.rules))
	}
	if sf.RemoveAll() {
		t.Errorf("second RemoveAll = true, want false (already empty)")
	}
}

func TestOptionsFor_DeletePartFiles(t *testing.T) {
	alice := keyID("tiktok", "alice")

	// No rule → nil, so the caller falls back to the global default.
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]")
	if got := sf.OptionsFor(alice).DeletePartFiles; got != nil {
		t.Errorf("DeletePartFiles with no rule = %v, want nil", got)
	}

	// delete_part_files = false rule → non-nil false override.
	sf = loadFile(t, `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
delete_part_files = false
`)
	if got := sf.OptionsFor(alice).DeletePartFiles; got == nil || *got != false {
		t.Errorf("DeletePartFiles = %v, want non-nil false", got)
	}
}

func TestRoundTrip_DeletePartFiles(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice"]
delete_part_files = false
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if len(sf2.rules) != 1 {
		t.Fatalf("Rules after round-trip = %d, want 1", len(sf2.rules))
	}
	if got := sf2.OptionsFor(keyID("tiktok", "alice")).DeletePartFiles; got == nil || *got != false {
		t.Errorf("DeletePartFiles after round-trip = %v, want non-nil false", got)
	}
}

func TestOptionsFor_OutputTemplate(t *testing.T) {
	alice := keyID("tiktok", "alice")

	// No rule → empty, so the caller falls back to the global default.
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]")
	if got := sf.OptionsFor(alice).OutputTemplate; got != "" {
		t.Errorf("OutputTemplate with no rule = %q, want empty", got)
	}

	// output_template rule → that value.
	sf = loadFile(t, `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
output_template = "{date}/{platform}-{username}"
`)
	if got := sf.OptionsFor(alice).OutputTemplate; got != "{date}/{platform}-{username}" {
		t.Errorf("OutputTemplate = %q, want the rule value", got)
	}
}

func TestRoundTrip_OutputTemplate(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice"]
output_template = "{year}/{username}"
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got := sf2.OptionsFor(keyID("tiktok", "alice")).OutputTemplate; got != "{year}/{username}" {
		t.Errorf("OutputTemplate after round-trip = %q, want {year}/{username}", got)
	}
}

func TestOptionsFor_Split(t *testing.T) {
	alice := keyID("tiktok", "alice")

	// No rule → empty, so the caller falls back to the global default.
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]")
	if opts := sf.OptionsFor(alice); opts.SplitSize != "" || opts.SplitDuration != "" {
		t.Errorf("split with no rule = (%q, %q), want both empty", opts.SplitSize, opts.SplitDuration)
	}

	// Valid values load verbatim (raw strings round-trip unparsed).
	sf = loadFile(t, `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
split_size = "2GB"
split_duration = "1h30m"
`)
	if opts := sf.OptionsFor(alice); opts.SplitSize != "2GB" || opts.SplitDuration != "1h30m" {
		t.Errorf("split = (%q, %q), want (2GB, 1h30m)", opts.SplitSize, opts.SplitDuration)
	}

	// The "off" disable token loads verbatim (it parses to "no limit").
	sf = loadFile(t, `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
split_size = "off"
`)
	if got := sf.OptionsFor(alice).SplitSize; got != "off" {
		t.Errorf("SplitSize = %q, want off", got)
	}
}

// Below-floor and garbage split values are warned about and treated as unset
// (inherit) on load, rather than failing the whole file.
func TestOptionsFor_SplitInvalidIgnored(t *testing.T) {
	alice := keyID("tiktok", "alice")
	tests := []struct {
		name string
		line string
	}{
		{"size below floor", `split_size = "5MB"`},
		{"size garbage", `split_size = "banana"`},
		{"duration below floor", `split_duration = "30s"`},
		{"duration garbage", `split_duration = "xyz"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sf := loadFile(t, `[streamers]
tiktok = ["alice"]

[[rules]]
tiktok = ["alice"]
`+tc.line+"\n")
			opts := sf.OptionsFor(alice)
			if opts.SplitSize != "" || opts.SplitDuration != "" {
				t.Errorf("invalid split loaded: (%q, %q), want both empty (ignored)", opts.SplitSize, opts.SplitDuration)
			}
		})
	}
}

func TestRoundTrip_Split(t *testing.T) {
	sf := loadFile(t, `
[[rules]]
tiktok = ["alice"]
split_size = "2GB"
split_duration = "off"
`)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	opts := sf2.OptionsFor(keyID("tiktok", "alice"))
	if opts.SplitSize != "2GB" {
		t.Errorf("SplitSize after round-trip = %q, want 2GB", opts.SplitSize)
	}
	if opts.SplitDuration != "off" {
		t.Errorf("SplitDuration after round-trip = %q, want off", opts.SplitDuration)
	}
}

// TestDropURLs_RemovesFromEverywhere covers the repair path for entries that
// name no streamer. They can be anywhere a URL is allowed, and a rule left with
// nothing in it must go too — an empty rule would keep its options alive and
// apply them to whatever was later added under the same shape.
func TestDropURLs_RemovesFromEverywhere(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://vimeo.com/12345", "https://www.twitch.tv/streamer_one"]
tiktok = ["alice"]

[[rules]]
urls = ["https://vimeo.com/999"]
monitor = false

[[rules]]
urls = ["https://vimeo.com/12345"]
tiktok = ["bob"]
variant = "720p"
`)

	dropped := sf.DropURLs([]string{"https://vimeo.com/12345", "https://vimeo.com/999"})
	slices.Sort(dropped)
	want := []string{"https://vimeo.com/12345", "https://vimeo.com/999"}
	if !slices.Equal(dropped, want) {
		t.Errorf("DropURLs returned %v, want %v", dropped, want)
	}

	got := idStrings(sf.AllIDs())
	wantIDs := []string{"https://www.twitch.tv/streamer_one", "tiktok.alice", "tiktok.bob"}
	if !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("AllIDs = %v, want %v", got, wantIDs)
	}
	// The rule that held only the dropped URL is gone; the one that also
	// covered a real streamer keeps its options.
	if len(sf.rules) != 1 {
		t.Fatalf("Rules = %d, want 1: %+v", len(sf.rules), sf.rules)
	}
	if v := sf.rules[0].Variant; v == nil || *v != "720p" {
		t.Errorf("surviving rule lost its options: %+v", sf.rules[0])
	}
	if len(sf.rules[0].uRLs) != 0 {
		t.Errorf("rule URLs = %v, want empty", sf.rules[0].uRLs)
	}
}

// TestDropURLs_ReportsOnlyWhatItFound matters because the caller logs the
// result as "removed".
func TestDropURLs_ReportsOnlyWhatItFound(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://vimeo.com/12345"]
`)
	dropped := sf.DropURLs([]string{
		"https://vimeo.com/12345",
		"https://vimeo.com/not-in-the-file",
		"",
	})
	if !slices.Equal(dropped, []string{"https://vimeo.com/12345"}) {
		t.Errorf("DropURLs returned %v, want just the entry that was there", dropped)
	}
}

// TestDropURLs_LeavesUsernameEntriesAlone guards the blast radius: a urls entry
// reading "platform.username" stringifies like the real platform/username
// entry, and must remove only itself.
func TestDropURLs_LeavesUsernameEntriesAlone(t *testing.T) {
	sf := loadFile(t, `
[streamers]
tiktok = ["alice"]
urls = ["tiktok.alice", "https://example.com/alice"]
`)
	dropped := sf.DropURLs([]string{"tiktok.alice"})

	if !slices.Equal(dropped, []string{"tiktok.alice"}) {
		t.Errorf("DropURLs returned %v, want the urls entry", dropped)
	}
	got := idStrings(sf.AllIDs())
	want := []string{"https://example.com/alice", "tiktok.alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllIDs = %v, want %v (the surviving tiktok.alice is the streamer, not the urls entry)",
			got, want)
	}
	// Spelled out, because the two survivors above stringify identically: what
	// is left must be the platform entry.
	for _, id := range sf.listed {
		if id.URL == "tiktok.alice" {
			t.Error("the urls entry survived")
		}
	}
	if len(sf.listed) != 2 {
		t.Errorf("Listed = %v, want the tiktok streamer and the example.com URL", sf.listed)
	}
}

// TestDropURLs_EmptiesFileCompletely covers a config whose only entry is junk:
// dropping it must leave an empty file rather than a repeat failure on every
// start.
func TestDropURLs_EmptiesFileCompletely(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = ["https://vimeo.com/12345"]
`)
	if dropped := sf.DropURLs([]string{"https://vimeo.com/12345"}); len(dropped) != 1 {
		t.Fatalf("DropURLs returned %v", dropped)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if ids := sf2.AllIDs(); len(ids) != 0 {
		t.Errorf("reloaded file lists %v, want nothing", ids.Strings())
	}
}

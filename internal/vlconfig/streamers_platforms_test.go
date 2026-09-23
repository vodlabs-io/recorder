package vlconfig

import (
	"os"
	"strings"
	"testing"
)

// reloadThroughWrite loads content with the given platform list in force, writes
// the file straight back, and returns the resulting bytes. Write serializes the
// in-memory model over the file, so anything the load did not keep is lost.
func reloadThroughWrite(t *testing.T, platforms []string, content string) string {
	t.Helper()
	snapshotPlatforms(t)
	ps := make([]Platform, len(platforms))
	for i, p := range platforms {
		ps[i] = Platform{Name: p}
	}
	SetPlatforms(ps)

	path := writeFile(t, content)
	sf, err := NewStreamersFile(path)
	if err != nil {
		t.Fatalf("NewStreamersFile: %v", err)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(out)
}

// TestWriteKeepsStreamersForUnlistedPlatforms checks that, with a short
// platform list, a load followed by a write keeps every streamer the list does
// not name.
func TestWriteKeepsStreamersForUnlistedPlatforms(t *testing.T) {
	got := reloadThroughWrite(t, []string{"tiktok", "kick"}, `
[streamers]
urls = []
tiktok = ['keeper']
twitch = ['streamer_one']
chzzk = ['someone']
`)
	for _, want := range []string{"tiktok", "keeper", "twitch", "streamer_one", "chzzk", "someone"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing after a write with a short platform list; file:\n%s", want, got)
		}
	}
}

// TestWriteKeepsRulesForUnlistedPlatforms checks that a rule whose only members
// are on an unlisted platform survives a write with its options.
func TestWriteKeepsRulesForUnlistedPlatforms(t *testing.T) {
	got := reloadThroughWrite(t, []string{"tiktok"}, `
[streamers]
urls = []
tiktok = ['keeper']

[[rules]]
variant = "720p"
twitch = ['streamer_nine']
`)
	for _, want := range []string{"variant", "720p", "twitch", "streamer_nine"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing after a write with a short platform list; file:\n%s", want, got)
		}
	}
}

// TestRuleOptionsAreNotReadAsPlatforms is the other side of parsing a rule by
// its own keys: every option must still be read as an option. If one were
// mistaken for a platform it would become a streamer, and if a platform were
// mistaken for an option its streamers would vanish.
func TestRuleOptionsAreNotReadAsPlatforms(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = []

[[rules]]
monitor = false
record = false
variant = "720p"
output_template = "{{.Username}}"
delete_part_files = true
split_size = "500MB"
split_duration = "1h"
archive_dir = "/tmp/vods"
tiktok = ['alice']
`)
	if len(sf.rules) != 1 {
		t.Fatalf("parsed %d rules, want 1", len(sf.rules))
	}
	r := sf.rules[0]
	if got := idStrings(r.ids()); len(got) != 1 || got[0] != "tiktok.alice" {
		t.Errorf("rule covers %v, want just tiktok.alice — an option was read as a platform", got)
	}
	for name, set := range map[string]bool{
		"monitor":           r.Monitor != nil,
		"record":            r.Record != nil,
		"variant":           r.Variant != nil,
		"output_template":   r.OutputTemplate != nil,
		"delete_part_files": r.DeletePartFiles != nil,
		"split_size":        r.SplitSize != nil,
		"split_duration":    r.SplitDuration != nil,
		"archive_dir":       r.ArchiveDir != nil,
	} {
		if !set {
			t.Errorf("option %q was not read; is it missing from ruleOptionKeys?", name)
		}
	}
}

// TestRuleOptionKeysMatchWriteTo pins ruleOptionKeys to the options that
// actually exist, so adding an option to rule_options.go without listing it
// fails here rather than turning into a phantom platform at parse time.
func TestRuleOptionKeysMatchWriteTo(t *testing.T) {
	// Every option set to something, so writeTo emits all of them.
	b, s := true, "x"
	all := RuleOptions{
		Monitor: &b, Record: &b, Variant: &s, OutputTemplate: &s,
		DeletePartFiles: &b, SplitSize: &s, SplitDuration: &s, ArchiveDir: &s,
	}
	written := all.writeTo(nil)
	if len(written) != len(ruleOptionKeys()) {
		t.Fatalf("writeTo emits %d options but ruleOptionKeys names %d",
			len(written), len(ruleOptionKeys()))
	}
	known := ruleOptionKeys()
	for _, kv := range written {
		if !contains(known, kv.key) {
			t.Errorf("option %q is written to the file but missing from ruleOptionKeys, "+
				"so parseRule would read it back as a platform", kv.key)
		}
	}
}

// TestUnknownPlatformKeysSortAfterKnownOnes pins the written order so the file
// does not churn: known platforms keep their familiar PlatformNames order and
// anything else follows alphabetically.
func TestUnknownPlatformKeysSortAfterKnownOnes(t *testing.T) {
	got := reloadThroughWrite(t, []string{"tiktok", "kick"}, `
[streamers]
urls = []
zzz = ['last']
kick = ['second']
aaa = ['first']
tiktok = ['leading']
`)
	order := []string{"tiktok", "kick", "aaa", "zzz"}
	at := make([]int, len(order))
	for i, k := range order {
		if at[i] = strings.Index(got, k+" = "); at[i] < 0 {
			t.Fatalf("%q missing from:\n%s", k, got)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Errorf("%q written before %q, want order %v; file:\n%s",
				order[i], order[i-1], order, got)
		}
	}
}

// TestWriteIsStableAcrossAShortPlatformList checks the file is byte-identical
// after a load/write cycle under a short platform list, so an unrelated Write
// is safe at any moment during startup.
func TestWriteIsStableAcrossAShortPlatformList(t *testing.T) {
	const content = `[streamers]
urls = []
tiktok = [
  'keeper'
]
twitch = [
  'streamer_one'
]

[[rules]]
record = false
twitch = [
  'streamer_one'
]
`
	if got := reloadThroughWrite(t, []string{"tiktok"}, content); got != content {
		t.Errorf("write was not idempotent under a short platform list:\n--- got\n%s\n--- want\n%s",
			got, content)
	}
}

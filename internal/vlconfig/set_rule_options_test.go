package vlconfig

import (
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

func bptr(b bool) *bool     { return &b }
func sptr(s string) *string { return &s }

func TestSetRuleOptions_Consolidates(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(alice, RuleOptions{
		Variant:         sptr("720p"),
		OutputTemplate:  sptr("{platform}/{username}"),
		DeletePartFiles: bptr(false),
	}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}

	if len(sf.rules) != 1 {
		t.Fatalf("len(Rules) = %d, want 1 (consolidated)", len(sf.rules))
	}
	opts := sf.OptionsFor(alice)
	if opts.Variant != "720p" {
		t.Errorf("Variant = %q, want 720p", opts.Variant)
	}
	if opts.OutputTemplate != "{platform}/{username}" {
		t.Errorf("OutputTemplate = %q, want {platform}/{username}", opts.OutputTemplate)
	}
	if opts.DeletePartFiles == nil || *opts.DeletePartFiles != false {
		t.Errorf("DeletePartFiles = %v, want non-nil false", opts.DeletePartFiles)
	}
	if !opts.Monitor {
		t.Errorf("Monitor = %v, want true (default, no override)", opts.Monitor)
	}
}

func TestSetRuleOptions_MonitorOneShotAndClear(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	// Monitor=false records a one-shot rule.
	if err := sf.SetRuleOptions(alice, RuleOptions{Monitor: bptr(false)}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if !sf.HasOneShotRule(alice) {
		t.Fatalf("expected one-shot rule after Monitor=false")
	}

	// Monitor=true is the default and clears the override.
	if err := sf.SetRuleOptions(alice, RuleOptions{Monitor: bptr(true)}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if sf.HasOneShotRule(alice) {
		t.Fatalf("expected no one-shot rule after Monitor=true")
	}
	if !sf.OptionsFor(alice).Monitor {
		t.Errorf("Monitor = false, want true (default)")
	}
	if len(sf.rules) != 0 {
		t.Errorf("len(Rules) = %d, want 0 after clearing", len(sf.rules))
	}
}

func TestSetRuleOptions_PreservesDisabled(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n\n[[rules]]\nrecord = false\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if sf.IsRecording(alice) {
		t.Fatalf("precondition: alice should not be recording")
	}
	if err := sf.SetRuleOptions(alice, RuleOptions{Variant: sptr("best")}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if sf.IsRecording(alice) {
		t.Errorf("record=false lost after SetRuleOptions")
	}
	if sf.OptionsFor(alice).Variant != "best" {
		t.Errorf("Variant = %q, want best", sf.OptionsFor(alice).Variant)
	}
}

func TestSetRuleOptions_ClearsVariant(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n\n[[rules]]\nvariant = \"720p\"\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(alice, RuleOptions{Variant: sptr("")}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if v := sf.OptionsFor(alice).Variant; v != "" {
		t.Errorf("Variant = %q, want cleared", v)
	}
	if len(sf.rules) != 0 {
		t.Errorf("len(Rules) = %d, want 0 after clearing the only option", len(sf.rules))
	}
}

func TestSetRuleOptions_PreservesSiblings(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\", \"bob\"]\n\n[[rules]]\nvariant = \"720p\"\ntiktok = [\"alice\", \"bob\"]\n")
	alice := keyID("tiktok", "alice")
	bob := keyID("tiktok", "bob")

	if err := sf.SetRuleOptions(alice, RuleOptions{Variant: sptr("1080p")}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if v := sf.OptionsFor(alice).Variant; v != "1080p" {
		t.Errorf("alice Variant = %q, want 1080p", v)
	}
	if v := sf.OptionsFor(bob).Variant; v != "720p" {
		t.Errorf("bob Variant = %q, want 720p (unchanged)", v)
	}
}

func TestSetRuleOptions_RoundTrip(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(alice, RuleOptions{
		Variant:         sptr("720p"),
		DeletePartFiles: bptr(false),
	}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	opts := sf2.OptionsFor(alice)
	if opts.Variant != "720p" {
		t.Errorf("Variant after round-trip = %q, want 720p", opts.Variant)
	}
	if opts.DeletePartFiles == nil || *opts.DeletePartFiles != false {
		t.Errorf("DeletePartFiles after round-trip = %v, want non-nil false", opts.DeletePartFiles)
	}
}

func TestSetRuleOptions_SplitFields(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(alice, RuleOptions{
		SplitSize:     sptr("2GB"),
		SplitDuration: sptr("1h30m"),
	}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if len(sf.rules) != 1 {
		t.Fatalf("len(Rules) = %d, want 1 (consolidated)", len(sf.rules))
	}
	opts := sf.OptionsFor(alice)
	if opts.SplitSize != "2GB" {
		t.Errorf("SplitSize = %q, want 2GB", opts.SplitSize)
	}
	if opts.SplitDuration != "1h30m" {
		t.Errorf("SplitDuration = %q, want 1h30m", opts.SplitDuration)
	}
}

// SetRuleOptions canonicalizes "0" to "off" and trims surrounding whitespace.
func TestSetRuleOptions_SplitNormalizes(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(alice, RuleOptions{
		SplitSize:     sptr("0"),
		SplitDuration: sptr("  90m  "),
	}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	opts := sf.OptionsFor(alice)
	if opts.SplitSize != "off" {
		t.Errorf("SplitSize = %q, want off (canonicalized from 0)", opts.SplitSize)
	}
	if opts.SplitDuration != "90m" {
		t.Errorf("SplitDuration = %q, want 90m (trimmed)", opts.SplitDuration)
	}
}

// An empty split string clears the override so the streamer inherits the global.
func TestSetRuleOptions_ClearsSplit(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n\n[[rules]]\nsplit_size = \"2GB\"\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if got := sf.OptionsFor(alice).SplitSize; got != "2GB" {
		t.Fatalf("precondition SplitSize = %q, want 2GB", got)
	}
	if err := sf.SetRuleOptions(alice, RuleOptions{SplitSize: sptr("")}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if got := sf.OptionsFor(alice).SplitSize; got != "" {
		t.Errorf("SplitSize after clear = %q, want empty", got)
	}
	if len(sf.rules) != 0 {
		t.Errorf("len(Rules) = %d, want 0 after clearing the only option", len(sf.rules))
	}
}

// A per-streamer "off" survives a write/reload round-trip (it parses cleanly).
func TestSetRuleOptions_SplitRoundTrip(t *testing.T) {
	sf := loadFile(t, "[streamers]\ntiktok = [\"alice\"]\n")
	alice := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(alice, RuleOptions{
		SplitSize:     sptr("2GB"),
		SplitDuration: sptr("off"),
	}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sf2, err := NewStreamersFile(sf.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	opts := sf2.OptionsFor(alice)
	if opts.SplitSize != "2GB" {
		t.Errorf("SplitSize after round-trip = %q, want 2GB", opts.SplitSize)
	}
	if opts.SplitDuration != "off" {
		t.Errorf("SplitDuration after round-trip = %q, want off", opts.SplitDuration)
	}
}

// A streamer that lives only inside a [[rules]] entry must survive having its
// last option cleared, since callers rely on an option edit never changing
// which streamers are tracked.
func TestSetRuleOptions_ClearingKeepsARulesOnlyStreamer(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = []

[[rules]]
tiktok = ["alice"]
variant = "720p"
`)
	id := keyID("tiktok", "alice")

	if err := sf.SetRuleOptions(id, RuleOptions{}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if got := sf.OptionsFor(id).Variant; got != "" {
		t.Errorf("variant = %q, want cleared", got)
	}
	if !containsID(sf.AllIDs(), id) {
		t.Errorf("clearing the last option dropped the streamer: %v", sf.AllIDs().Strings())
	}
}

// Same property through the variant endpoint's own path.
func TestSetVariantRule_ClearingKeepsARulesOnlyStreamer(t *testing.T) {
	sf := loadFile(t, `
[streamers]
urls = []

[[rules]]
tiktok = ["alice"]
variant = "720p"
`)
	id := keyID("tiktok", "alice")

	if err := sf.SetVariantRule(id, ""); err != nil {
		t.Fatalf("SetVariantRule: %v", err)
	}
	if !containsID(sf.AllIDs(), id) {
		t.Errorf("clearing the variant dropped the streamer: %v", sf.AllIDs().Strings())
	}
}

func containsID(ids streamer.StreamerRefs, id streamer.StreamerRef) bool {
	for _, got := range ids {
		if got.String() == id.String() {
			return true
		}
	}
	return false
}

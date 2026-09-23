package vlconfig

import "testing"

// Switching recording back on must not take the streamer's other overrides
// with it: SetRuleOptions folds them into the same rule as the record flag.
func TestRemoveRecordOffRule_KeepsSiblingOptions(t *testing.T) {
	sf := loadFile(t, "[streamers]\nyoutube = [\"streamer_six\"]\n\n[[rules]]\nrecord = false\nyoutube = [\"streamer_six\"]\n")
	id := keyID("youtube", "streamer_six")

	if err := sf.SetRuleOptions(id, RuleOptions{
		Variant:   sptr("best"),
		SplitSize: sptr("2GB"),
	}); err != nil {
		t.Fatalf("SetRuleOptions: %v", err)
	}
	if opts := sf.OptionsFor(id); opts.Record {
		t.Fatalf("Record = true, want false (preserved by SetRuleOptions)")
	}

	if !sf.RemoveRecordOffRule(id) {
		t.Fatal("RemoveRecordOffRule = false, want true")
	}
	opts := sf.OptionsFor(id)
	if !opts.Record {
		t.Errorf("Record = false, want true")
	}
	if opts.Variant != "best" {
		t.Errorf("Variant = %q, want best", opts.Variant)
	}
	if opts.SplitSize != "2GB" {
		t.Errorf("SplitSize = %q, want 2GB", opts.SplitSize)
	}
}

// The record flag is cleared for the named streamer only; everyone else in the
// rule keeps it, along with the options the rule carries.
func TestRemoveRecordOffRule_SiblingsUnaffected(t *testing.T) {
	sf := loadFile(t, "[streamers]\nyoutube = [\"streamer_seven\", \"streamer_six\"]\n\n[[rules]]\nrecord = false\nvariant = \"best\"\nyoutube = [\"streamer_seven\", \"streamer_six\"]\n")
	id := keyID("youtube", "streamer_six")
	sibling := keyID("youtube", "streamer_seven")

	if !sf.RemoveRecordOffRule(id) {
		t.Fatal("RemoveRecordOffRule = false, want true")
	}
	if opts := sf.OptionsFor(sibling); opts.Record || opts.Variant != "best" {
		t.Errorf("sibling opts = {Record:%v Variant:%q}, want {false best}", opts.Record, opts.Variant)
	}
	if opts := sf.OptionsFor(id); !opts.Record || opts.Variant != "best" {
		t.Errorf("opts = {Record:%v Variant:%q}, want {true best}", opts.Record, opts.Variant)
	}
}

// Clearing a variant must not switch recording back on: SetVariantRule removes
// the old variant rule first, and that rule can also carry the record flag.
func TestSetVariantRule_KeepsRecordOff(t *testing.T) {
	sf := loadFile(t, "[streamers]\nyoutube = [\"streamer_six\"]\n\n[[rules]]\nrecord = false\nvariant = \"best\"\nyoutube = [\"streamer_six\"]\n")
	id := keyID("youtube", "streamer_six")

	if err := sf.SetVariantRule(id, "720p"); err != nil {
		t.Fatalf("SetVariantRule: %v", err)
	}
	opts := sf.OptionsFor(id)
	if opts.Record {
		t.Errorf("Record = true, want false (untouched by a variant change)")
	}
	if opts.Variant != "720p" {
		t.Errorf("Variant = %q, want 720p", opts.Variant)
	}

	if err := sf.SetVariantRule(id, ""); err != nil {
		t.Fatalf("SetVariantRule clear: %v", err)
	}
	opts = sf.OptionsFor(id)
	if opts.Record {
		t.Errorf("Record = true after clearing variant, want false")
	}
	if opts.Variant != "" {
		t.Errorf("Variant = %q, want empty", opts.Variant)
	}
}

// The carried options fold back into the rule that already holds them, so a
// round trip through record off/on leaves the file no larger than it started.
func TestRemoveRecordOffRule_NoRuleGrowth(t *testing.T) {
	sf := loadFile(t, "[streamers]\nyoutube = [\"streamer_seven\", \"streamer_six\"]\n\n[[rules]]\nrecord = false\nvariant = \"best\"\nyoutube = [\"streamer_seven\", \"streamer_six\"]\n\n[[rules]]\nvariant = \"best\"\nyoutube = [\"streamer_eight\"]\n")
	id := keyID("youtube", "streamer_six")

	sf.RemoveRecordOffRule(id)
	if err := sf.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(sf.rules) != 2 {
		t.Errorf("len(rules) = %d, want 2", len(sf.rules))
	}
}

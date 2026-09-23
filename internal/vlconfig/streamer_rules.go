package vlconfig

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// ids returns the streamer IDs covered by this rule.
func (r streamerRule) ids() streamer.StreamerRefs {
	var ids streamer.StreamerRefs
	for platform, users := range r.usernames {
		for _, u := range users {
			if u == "" {
				continue
			}
			ids = append(ids, streamer.StreamerRef{
				StreamerKey: streamer.StreamerKey{Platform: platform, Username: u},
			})
		}
	}
	for _, u := range r.uRLs {
		if u == "" {
			continue
		}
		ids = append(ids, streamer.StreamerRef{URL: u})
	}
	return ids
}

// matches reports whether this rule covers the given streamer ID. Rule
// usernames are canonical (loadListed/parseRule fold them), so the incoming ID
// is folded too rather than compared as typed.
func (r streamerRule) matches(id streamer.StreamerRef) bool {
	id = id.Canonical()
	if id.StreamerKey.Valid() {
		if users, ok := r.usernames[id.Platform]; ok && slices.Contains(users, id.Username) {
			return true
		}
	}
	if id.URL != "" && slices.Contains(r.uRLs, id.URL) {
		return true
	}
	return false
}

// isEmpty reports whether the rule covers no streamers.
func (r streamerRule) isEmpty() bool {
	return len(r.usernames) == 0 && len(r.uRLs) == 0
}

// addID adds id to the rule's usernames or URLs, canonically so that later
// matches/removeID lookups (and the [streamers] list) key on the same identity.
func (r *streamerRule) addID(id streamer.StreamerRef) error {
	id = id.Canonical()
	switch {
	case id.StreamerKey.Valid():
		if r.usernames == nil {
			r.usernames = map[string][]string{}
		}
		r.usernames[id.Platform] = append(r.usernames[id.Platform], id.Username)
	case id.URL != "":
		r.uRLs = append(r.uRLs, id.URL)
	default:
		return fmt.Errorf("invalid streamer ID")
	}
	return nil
}

// removeID strips id from the rule's usernames and URLs. Returns true if
// anything was removed. Empty platform buckets are pruned.
func (r *streamerRule) removeID(id streamer.StreamerRef) bool {
	id = id.Canonical()
	changed := false
	if id.StreamerKey.Valid() {
		if users, ok := r.usernames[id.Platform]; ok {
			before := len(users)
			users = slices.DeleteFunc(users, func(u string) bool { return u == id.Username })
			if len(users) < before {
				changed = true
			}
			if len(users) == 0 {
				delete(r.usernames, id.Platform)
			} else {
				r.usernames[id.Platform] = users
			}
		}
	}
	if id.URL != "" {
		before := len(r.uRLs)
		r.uRLs = slices.DeleteFunc(r.uRLs, func(u string) bool { return u == id.URL })
		if len(r.uRLs) < before {
			changed = true
		}
	}
	return changed
}

// toKVs encodes the rule as ordered TOML pairs: options first (delegated to
// RuleOptions.writeTo, so adding an option only touches rule_options.go), then
// urls, then platform username lists. Options stay above the lists to match the
// [streamers] layout and keep rules readable.
func (r streamerRule) toKVs() []tomlKV {
	kvs := r.RuleOptions.writeTo(nil)
	if len(r.uRLs) > 0 {
		kvs = append(kvs, tomlKV{urlsKey, r.uRLs})
	}
	// Every platform the rule covers, not just the ones currently supported —
	// see listedToKVs for why the platform list must not decide what is written.
	for _, p := range platformKeyOrder(r.usernames) {
		kvs = append(kvs, tomlKV{p, r.usernames[p]})
	}
	return kvs
}

// consolidateRules merges rules that carry identical options into a single rule,
// keeping the order of first appearance. Rules whose options differ stay
// separate, and a streamer listed under more than one mergeable rule is included
// only once. Each merged rule is rebuilt from scratch so it never aliases the
// input rules' maps or slices. Empty rules are dropped.
//
// This runs on every Write, so the file is normalized the same way whether the
// rules were produced by daemon mutations or hand-edited in a text editor.
func consolidateRules(rules []streamerRule) []streamerRule {
	merged := make([]streamerRule, 0, len(rules))
	for _, r := range rules {
		ids := r.ids()
		if len(ids) == 0 {
			continue
		}
		idx := -1
		for i := range merged {
			if merged[i].RuleOptions.equal(r.RuleOptions) {
				idx = i
				break
			}
		}
		if idx < 0 {
			nr := streamerRule{RuleOptions: r.RuleOptions}
			for _, id := range ids {
				nr.addID(id)
			}
			merged = append(merged, nr)
			continue
		}
		for _, id := range ids {
			if !merged[idx].matches(id) {
				merged[idx].addID(id)
			}
		}
	}
	return merged
}

// newRuleFor builds a fresh rule covering exactly id.
func newRuleFor(id streamer.StreamerRef) (streamerRule, error) {
	var r streamerRule
	if err := r.addID(id); err != nil {
		return r, err
	}
	return r, nil
}

// OptionsFor returns the effective options for the given ID after walking the
// rules. Default Monitor is true.
func (sf *StreamersFile) OptionsFor(id streamer.StreamerRef) streamerOptions {
	sf.mu.RLock()
	defer sf.mu.RUnlock()
	return sf.optionsForLocked(id)
}

// optionsForLocked is OptionsFor for callers that already hold sf.mu.
func (sf *StreamersFile) optionsForLocked(id streamer.StreamerRef) streamerOptions {
	opts := streamerOptions{Monitor: true, Record: true}
	for _, r := range sf.rules {
		if !r.matches(id) {
			continue
		}
		r.applyTo(&opts) // promoted from embedded RuleOptions
	}
	return opts
}

// IsRecording reports whether the effective options for id allow recording.
func (sf *StreamersFile) IsRecording(id streamer.StreamerRef) bool {
	return sf.OptionsFor(id).Record
}

// isRecordingLocked reports id's effective record state (last matching rule
// wins). The caller must hold sf.mu.
func (sf *StreamersFile) isRecordingLocked(id streamer.StreamerRef) bool {
	record := true
	for _, r := range sf.rules {
		if r.matches(id) && r.Record != nil {
			record = *r.Record
		}
	}
	return record
}

// RecordOffAll adds a single record=false rule covering every currently-covered
// streamer still set to record, so switching the whole list off touches the
// file once instead of once per streamer. Returns true if anything changed.
func (sf *StreamersFile) RecordOffAll() bool {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	seen := make(map[string]struct{})
	var toStop streamer.StreamerRefs
	consider := func(id streamer.StreamerRef) {
		s := id.String()
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		if sf.isRecordingLocked(id) {
			toStop = append(toStop, id)
		}
	}
	for _, id := range sf.listed {
		consider(id)
	}
	for _, r := range sf.rules {
		for _, id := range r.ids() {
			consider(id)
		}
	}
	if len(toStop) == 0 {
		return false
	}

	record := false
	r := streamerRule{RuleOptions: RuleOptions{Record: &record}}
	for _, id := range toStop {
		_ = r.addID(id)
	}
	sf.rules = append(sf.rules, r)
	return true
}

// relistOrphans re-homes into [streamers] any of the given IDs that are no
// longer tracked anywhere, neither in sf.listed nor covered by a remaining
// rule, so a streamer isn't removed along with a dropped rule. The caller must
// hold sf.mu and must have already updated sf.rules.
func (sf *StreamersFile) relistOrphans(ids streamer.StreamerRefs) {
	tracked := make(map[string]struct{})
	for _, id := range sf.listed {
		tracked[id.String()] = struct{}{}
	}
	for _, r := range sf.rules {
		for _, id := range r.ids() {
			tracked[id.String()] = struct{}{}
		}
	}
	for _, id := range ids {
		s := id.String()
		if s == "" {
			continue
		}
		if _, ok := tracked[s]; ok {
			continue
		}
		tracked[s] = struct{}{}
		sf.listed = append(sf.listed, id)
	}
}

// RecordOnAll clears record=false from every rule in one pass. A rule that
// carries no other options once the flag is removed is dropped; a rule with
// additional options keeps them. A streamer that existed only inside a dropped
// rule is re-homed into [streamers] so switching recording back on never
// removes it. Returns true if anything changed.
func (sf *StreamersFile) RecordOnAll() bool {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	changed := false
	var orphaned streamer.StreamerRefs
	kept := make([]streamerRule, 0, len(sf.rules))
	for _, r := range sf.rules {
		if r.Record != nil && !*r.Record {
			r.Record = nil
			changed = true
			if !r.RuleOptions.anySet() {
				// Pure record=false rule → drop it, but keep its streamers.
				orphaned = append(orphaned, r.ids()...)
				continue
			}
		}
		kept = append(kept, r)
	}
	sf.rules = kept
	sf.relistOrphans(orphaned)
	return changed
}

// RemoveAll clears every streamer from both [streamers] and [[rules]]. Returns
// true if anything was present to remove.
func (sf *StreamersFile) RemoveAll() bool {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	if len(sf.listed) == 0 && len(sf.rules) == 0 {
		return false
	}
	sf.listed = nil
	sf.rules = nil
	return true
}

// HasOneShotRule reports whether the file already has a monitor=false rule
// covering the given ID.
func (sf *StreamersFile) HasOneShotRule(id streamer.StreamerRef) bool {
	sf.mu.RLock()
	defer sf.mu.RUnlock()

	for _, r := range sf.rules {
		if r.Monitor != nil && !*r.Monitor && r.matches(id) {
			return true
		}
	}
	return false
}

// AddOneShotRule appends a new [[rules]] entry with monitor=false for id.
// Each call appends a separate rule rather than merging into an existing one,
// keeping the file easy to read and edit.
func (sf *StreamersFile) AddOneShotRule(id streamer.StreamerRef) error {
	if sf.HasOneShotRule(id) {
		return fmt.Errorf("rule already exists")
	}
	r, err := newRuleFor(id)
	if err != nil {
		return err
	}
	monitor := false
	r.Monitor = &monitor

	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.rules = append(sf.rules, r)
	return nil
}

// AddRecordOffRule appends a new [[rules]] entry with record=false for id.
// Each call appends a separate rule rather than merging into an existing one,
// mirroring AddOneShotRule.
func (sf *StreamersFile) AddRecordOffRule(id streamer.StreamerRef) error {
	r, err := newRuleFor(id)
	if err != nil {
		return err
	}
	record := false
	r.Record = &record

	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.rules = append(sf.rules, r)
	return nil
}

// clearOptionFor switches one option back to inherit for a single streamer:
// every rule that sel picks and that covers id drops id, and whatever else
// those rules set for it is re-attached as a fresh rule. unset zeroes the
// option being cleared on that carried copy. Rules left empty are dropped, and
// sibling entries keep everything they had. Returns true if any rule matched.
//
// The carry-over matters because SetRuleOptions folds a streamer's overrides
// into one rule; Write's consolidateRules merges the carried rule back, so no
// extra [[rules]] entry results. The caller must hold sf.mu.
func (sf *StreamersFile) clearOptionFor(id streamer.StreamerRef, sel func(streamerRule) bool, unset func(*RuleOptions)) bool {
	changed := false
	kept := make([]streamerRule, 0, len(sf.rules))
	var carried []RuleOptions
	for _, r := range sf.rules {
		if !sel(r) || !r.matches(id) {
			kept = append(kept, r)
			continue
		}
		r.removeID(id)
		changed = true
		rest := r.RuleOptions
		unset(&rest)
		if rest.anySet() {
			carried = append(carried, rest)
		}
		if r.isEmpty() {
			continue
		}
		kept = append(kept, r)
	}
	sf.rules = kept
	for _, opts := range carried {
		nr, err := newRuleFor(id)
		if err != nil {
			continue
		}
		nr.RuleOptions = opts
		sf.rules = append(sf.rules, nr)
	}
	return changed
}

// RemoveRecordOffRule switches id back on by clearing the record flag from any
// record=false rule that covers it. Sibling entries in the same rule keep their
// record flag, and id keeps every other option those rules set for it. Rules
// left empty are dropped. If id existed only inside the dropped rule it is
// re-homed into [streamers] so switching recording on never removes it.
func (sf *StreamersFile) RemoveRecordOffRule(id streamer.StreamerRef) bool {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	changed := sf.clearOptionFor(id,
		func(r streamerRule) bool { return r.Record != nil && !*r.Record },
		func(o *RuleOptions) { o.Record = nil })
	if changed {
		sf.relistOrphans(streamer.StreamerRefs{id})
	}
	return changed
}

// SetVariantRule sets the variant override for id. Any existing variant rule
// covering id is removed first; if variant is non-empty, a new [[rules]] entry
// with variant=<variant> is appended for id. An empty variant clears the
// override.
func (sf *StreamersFile) SetVariantRule(id streamer.StreamerRef, variant string) error {
	sf.RemoveVariantRule(id)
	if variant == "" {
		// Clearing the override must not remove the streamer: one that existed
		// only inside the variant rule would otherwise leave the file along
		// with it. The same guarantee RemoveRecordOffRule gives.
		sf.mu.Lock()
		sf.relistOrphans(streamer.StreamerRefs{id})
		sf.mu.Unlock()
		return nil
	}
	r, err := newRuleFor(id)
	if err != nil {
		return err
	}
	r.Variant = &variant

	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.rules = append(sf.rules, r)
	return nil
}

// RemoveVariantRule clears id's variant override from any rule that sets one.
// Sibling entries in the same rule keep their variant, and id keeps every other
// option those rules set for it — clearing a variant must not switch recording
// back on. Rules left empty are dropped. Returns true if anything was removed.
func (sf *StreamersFile) RemoveVariantRule(id streamer.StreamerRef) bool {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	return sf.clearOptionFor(id,
		func(r streamerRule) bool { return r.Variant != nil },
		func(o *RuleOptions) { o.Variant = nil })
}

// SetRuleOptions replaces id's option overrides with the values set in opts,
// consolidating them into a single [[rules]] entry, while preserving any
// existing record override for id (record is owned by
// AddRecordOffRule/RemoveRecordOffRule).
//
// opts is the complete desired state: an inherit-equivalent value (nil, a true
// Monitor, or an empty string) clears the override.
func (sf *StreamersFile) SetRuleOptions(id streamer.StreamerRef, opts RuleOptions) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	// Preserve any existing record override for id (last match wins).
	for _, r := range sf.rules {
		if r.matches(id) && r.Record != nil {
			v := *r.Record
			opts.Record = &v
		}
	}

	// Normalize inherit-equivalents (and the default record=true) to unset.
	if opts.Monitor != nil && *opts.Monitor {
		opts.Monitor = nil
	}
	if opts.Variant != nil && *opts.Variant == "" {
		opts.Variant = nil
	}
	if opts.OutputTemplate != nil && *opts.OutputTemplate == "" {
		opts.OutputTemplate = nil
	}
	if opts.Record != nil && *opts.Record {
		opts.Record = nil
	}
	normSplitStr := func(p **string) {
		if *p == nil {
			return
		}
		v := strings.TrimSpace(**p)
		if v == "" {
			*p = nil // empty = inherit
			return
		}
		if v == "0" {
			v = "off" // canonical disable token
		}
		*p = &v
	}
	normSplitStr(&opts.SplitSize)
	normSplitStr(&opts.SplitDuration)

	// Strip id from every existing rule; drop any left empty.
	kept := make([]streamerRule, 0, len(sf.rules))
	for _, r := range sf.rules {
		r.removeID(id)
		if !r.isEmpty() {
			kept = append(kept, r)
		}
	}
	sf.rules = kept

	// Append one consolidated rule for id when any override remains.
	if opts.anySet() {
		r, err := newRuleFor(id)
		if err != nil {
			return err
		}
		r.RuleOptions = opts
		sf.rules = append(sf.rules, r)
	}
	// Setting every option back to inherit must not remove a streamer that
	// existed only inside its rule, so an option edit never changes what is
	// tracked.
	sf.relistOrphans(streamer.StreamerRefs{id})
	return nil
}

// OneShotPurge pairs a one-shot streamer purged from the file with the
// effective options it had at purge time. The file no longer remembers those
// options after the purge, so the caller carries them into the KV one-shot
// list.
type OneShotPurge struct {
	ID   streamer.StreamerRef
	Opts streamerOptions
}

// PurgeOneShots removes every streamer covered by a monitor=false rule from
// both [streamers] and [[rules]]. Rules left empty after purging are dropped.
// Key-based (platform/username) entries that are removed are returned along
// with their pre-purge effective options; URL-based entries are dropped
// silently.
func (sf *StreamersFile) PurgeOneShots() []OneShotPurge {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	var purgeIDs streamer.StreamerRefs
	var removed []OneShotPurge
	seen := make(map[string]struct{})
	for _, r := range sf.rules {
		if r.Monitor == nil || *r.Monitor {
			continue
		}
		for _, id := range r.ids() {
			s := id.String()
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			purgeIDs = append(purgeIDs, id)
			if id.StreamerKey.Valid() {
				removed = append(removed, OneShotPurge{ID: id, Opts: sf.optionsForLocked(id)})
			}
		}
	}
	if len(purgeIDs) == 0 {
		return removed
	}

	keptListed := make(streamer.StreamerRefs, 0, len(sf.listed))
	for _, id := range sf.listed {
		if _, drop := seen[id.String()]; drop {
			continue
		}
		keptListed = append(keptListed, id)
	}
	sf.listed = keptListed

	keptRules := make([]streamerRule, 0, len(sf.rules))
	for _, r := range sf.rules {
		for _, pid := range purgeIDs {
			r.removeID(pid)
		}
		if r.isEmpty() {
			continue
		}
		keptRules = append(keptRules, r)
	}
	sf.rules = keptRules
	return removed
}

// Normalize applies URL-to-Key resolutions (from a cloud subscribe response)
// to both the [streamers] section and the [[rules]] section. URL entries with
// no matching key in `resolved` are left in place.
func (sf *StreamersFile) Normalize(resolved streamer.StreamerRefs) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	urlToKey := make(map[string]streamer.StreamerKey, len(resolved))
	for _, id := range resolved {
		if id.URL != "" && id.StreamerKey.Valid() {
			urlToKey[id.URL] = id.StreamerKey
		}
	}

	// Resolve URLs in sf.listed.
	var newListed streamer.StreamerRefs
	seen := make(map[string]struct{}, len(sf.listed))
	for _, id := range sf.listed {
		if id.URL != "" {
			if k, ok := urlToKey[id.URL]; ok {
				id = streamer.StreamerRef{StreamerKey: k}
			}
		}
		s := id.String()
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		newListed = append(newListed, id)
	}
	sf.listed = newListed

	sf.rules = normalizeRulesWithMap(urlToKey, sf.rules)
}

func normalizeRulesWithMap(urlToKey map[string]streamer.StreamerKey, rules []streamerRule) []streamerRule {
	if len(rules) == 0 {
		return rules
	}
	var newRules []streamerRule
	for _, r := range rules {
		if len(r.uRLs) == 0 {
			newRules = append(newRules, r)
			continue
		}
		if r.usernames == nil {
			r.usernames = map[string][]string{}
		}
		var unresolved []string
		for _, u := range r.uRLs {
			if k, ok := urlToKey[u]; ok {
				r.usernames[k.Platform] = append(r.usernames[k.Platform], k.Username)
			} else {
				unresolved = append(unresolved, u)
			}
		}
		r.uRLs = unresolved
		if len(r.uRLs) > 0 || len(r.usernames) > 0 {
			newRules = append(newRules, r)
		}
	}
	return newRules
}

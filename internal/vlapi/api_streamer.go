package vlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// MARK: parseStreamerID
// parseStreamerID unmarshals the request payload into a StreamerRef and validates it.
// Returns the parsed ID, or responds with an error and returns ok=false.
func parseStreamerID(req micro.Request) (streamer.StreamerRef, bool) {
	var id streamer.StreamerRef
	if err := json.Unmarshal(req.Data(), &id); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return id, false
	}
	return validateStreamerID(req, id)
}

// MARK: validateStreamerID
// validateStreamerID validates an already-unmarshalled StreamerRef. Returns
// the ID and ok=true on success; otherwise responds with an error and returns
// ok=false.
func validateStreamerID(req micro.Request, id streamer.StreamerRef) (streamer.StreamerRef, bool) {
	hasKey := id.StreamerKey.Platform != "" || id.StreamerKey.Username != ""
	hasURL := id.URL != ""
	if hasKey && hasURL {
		respond(req, fail(errInvalidArg, "specify either platform/username or url, not both"))
		return id, false
	}
	if hasURL {
		if !looksLikeWebAddress(id.URL) {
			respond(req, fail(errInvalidArg,
				"That doesn't look like a web address. Paste the page you see while watching the stream."))
			return id, false
		}
		return id, true
	}
	if id.StreamerKey.Platform == "" {
		respond(req, fail(errInvalidArg, "missing platform"))
		return id, false
	}
	if !slices.Contains(vlconfig.PlatformNames(), id.StreamerKey.Platform) {
		respond(req, fail(errInvalidArg, "unsupported platform"))
		return id, false
	}
	if id.StreamerKey.Username == "" {
		respond(req, fail(errInvalidArg, "missing username"))
		return id, false
	}
	// Canonicalize so the stored entry, its view doc, and later requests all
	// key on the same identity.
	return id.Canonical(), true
}

// looksLikeWebAddress is a cheap sanity check so obvious nonsense is refused
// without a cloud round trip; whether a URL names a streamer is the cloud's
// call. A missing scheme is tolerated because browsers hide it
// ("twitch.tv/streamer_one").
func looksLikeWebAddress(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if !strings.Contains(s, "://") {
		s = "https://" + strings.TrimPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	// A host with no dot is not a site anyone pasted from a browser; it is a
	// typo, or a bare word the user meant as a username.
	return strings.Contains(u.Hostname(), ".")
}

// addResolveTimeout bounds how long an add waits for the cloud to name the
// streamer and confirm it exists. The cloud gives up first, so reaching this
// means the cloud itself is unreachable.
const addResolveTimeout = 15 * time.Second

// monitorUnsupportedMessage refuses a monitored add of a streamer that can only
// be recorded once.
const monitorUnsupportedMessage = "VodLabs has no support for this site yet, so it can only record what is live there now. Switch Monitor off to record it once."

// kvLookupTimeout bounds a read of the local view store. The bucket is served
// by this machine's own NATS, so anything slower than this is a fault, not
// latency, and the caller has a safe answer to fall back on.
const kvLookupTimeout = 2 * time.Second

// preflight asks the cloud to name a streamer and confirm it exists, returning
// the canonical identity to store. It runs before anything is written, so a URL
// that names no streamer never reaches streamers.toml. Every failure refuses
// the add, including an unreachable cloud, since status comes only from the
// cloud. On failure it responds to req and returns ok=false.
func (s *Service) preflight(req micro.Request, id streamer.StreamerRef) (streamer.StreamerRef, bool) {
	if s.streamerFuncs.Resolve == nil {
		// No cloud client wired (tests, or a daemon running without one). There
		// is nothing to check against, so take the request at its word.
		return id, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), addResolveTimeout)
	defer cancel()

	resolved, err := s.streamerFuncs.Resolve(ctx, id)
	if err != nil {
		respond(req, fail(rejectionCode(err), err.Error()))
		return streamer.StreamerRef{}, false
	}
	return resolved, true
}

// resolveURLRef turns a URL-only ref into the platform/username identity the
// recording will actually carry. A rule keyed on a URL matches nothing at
// record time — streamerRule.matches compares a resolved ID, whose URL field is
// empty — so every handler that writes or clears a rule has to resolve first.
//
// wasURL reports that the identity changed, the caller's cue to also drop any
// rule keyed on the raw URL.
func (s *Service) resolveURLRef(
	req micro.Request,
	id streamer.StreamerRef,
) (resolved streamer.StreamerRef, wasURL bool, ok bool) {
	if id.Canonical().StreamerKey.Valid() {
		return id, false, true
	}
	resolved, ok = s.preflight(req, id)
	if !ok {
		return streamer.StreamerRef{}, false, false
	}
	return resolved, resolved.String() != id.String(), true
}

// rejectionCode maps a preflight failure onto the vlapi error code clients
// switch on. The verdict is read through an interface so vlapi does not depend
// on the cloud package.
func rejectionCode(err error) error {
	var verdict interface{ Definitive() bool }
	if errors.As(err, &verdict) && !verdict.Definitive() {
		// The cloud could not answer. Retrying later may well succeed, which is
		// a different thing from the streamer not existing.
		return errUnavailable
	}
	return ErrNotFound
}

// AddRequest is the payload for streamer.add. Variant is optional; when set,
// the override is written as part of the add itself rather than by a follow-up
// set_variant call. The embedded ref inlines, so a payload carrying only the
// identity fields is still a valid request.
type AddRequest struct {
	streamer.StreamerRef
	Variant string `json:"variant"`
}

// AddResponse is what streamer.add and streamer.record_once reply with: the
// canonical platform/username the streamer was tracked under, which a URL add
// has no other way to learn.
type AddResponse struct {
	streamer.StreamerRef
}

// MARK: Add
// handleAddStreamer processes requests to add a streamer to the monitoring list.
// A URL that names no streamer (an unsupported site, a Platform VOD link, a
// deleted account) is refused by the preflight.
//
// A variant override travels with the add so it is committed before the
// subscription cycle starts; a follow-up call would race a recording of a
// streamer who is already live.
func (s *Service) handleAddStreamer(req micro.Request) {
	var payload AddRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	id, ok := validateStreamerID(req, payload.StreamerRef)
	if !ok {
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	// Checked before the round trip on what the user typed, and again after on
	// what it resolved to: a URL and a platform/username naming the same
	// streamer look nothing alike until the cloud has decoded the URL.
	if s.alreadyAdded(id) {
		respond(req, fail(ErrConflict, "already added"))
		return
	}

	id, ok = s.preflight(req, id)
	if !ok {
		return
	}
	if vlconfig.NoMonitor(id.StreamerKey.Platform) {
		respond(req, fail(ErrMonitorUnsupported, monitorUnsupportedMessage))
		return
	}
	if s.alreadyAdded(id) {
		respond(req, fail(ErrConflict, "already added"))
		return
	}

	// Before AddID so a rejected variant leaves nothing half-applied in the
	// in-memory file.
	if payload.Variant != "" {
		if err := s.streamersFile.SetVariantRule(id, payload.Variant); err != nil {
			respond(req, fail(errInvalidArg, err.Error()))
			return
		}
	}
	s.streamersFile.AddID(id)
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}
	s.markAdded(id)

	// The status fetch is left to applySubscriptionDiff: the cloud refuses a
	// status request for a streamer not yet subscribed.
	s.resubscribe()
	respond(req, OK(AddResponse{StreamerRef: id}))
}

// markAdded restarts the streamer's added time. Called only once the add is
// written, and before the resubscribe, so the docs that cycle builds already
// carry the new time.
func (s *Service) markAdded(id streamer.StreamerRef) {
	id = id.Canonical()
	if s.added == nil || !id.StreamerKey.Valid() {
		return
	}
	s.added.MarkAdded(context.Background(), id.StreamerKey, time.Now())
}

// alreadyAdded reports whether streamers.toml already lists this streamer.
// The file must have been loaded first.
func (s *Service) alreadyAdded(id streamer.StreamerRef) bool {
	target := id.String()
	if target == "" {
		return false
	}
	for _, existing := range s.streamersFile.AllIDs() {
		if existing.String() == target {
			return true
		}
	}
	return false
}

// oneShotConflict reports whether a record-once request names a streamer that
// is already tracked, and how to say so.
//
// A monitored streamer is refused rather than converted: the conversion would
// delete it from streamers.toml once the run finished.
func (s *Service) oneShotConflict(id streamer.StreamerRef) (string, bool) {
	if s.isOneShot(id) {
		return "already added as one-shot", true
	}
	if s.alreadyAdded(id) {
		return "already added — it is monitored, so it already records every run", true
	}
	return "", false
}

// MARK: Remove
// handleRemoveStreamer processes requests to remove a streamer from the monitoring list.
func (s *Service) handleRemoveStreamer(req micro.Request) {
	var id streamer.StreamerRef
	if err := json.Unmarshal(req.Data(), &id); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	// RemoveID also folds case, so hand-written mixed-case entries still match.
	id = id.Canonical()
	hasKey := id.StreamerKey.Valid()
	hasURL := id.URL != ""
	if !hasKey && !hasURL {
		respond(req, fail(errInvalidArg, "missing platform/username or url"))
		return
	}
	if hasKey && !slices.Contains(vlconfig.PlatformNames(), id.StreamerKey.Platform) {
		respond(req, fail(errInvalidArg, "unsupported platform"))
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	// A URL has to become platform/username before anything can be found by it:
	// the add resolved it, so that is the identity streamers.toml and the KV
	// one-shot list are keyed on.
	typed := id
	resolved, wasURL, ok := s.resolveURLRef(req, id)
	if !ok {
		return
	}
	id = resolved

	// A one-shot streamer lives only in the KV one-shot list (it's been purged
	// from streamers.toml), so RemoveID alone misses it. Drop it from the KV list
	// too; the resubscribe below then unsubscribes it, cancelling any in-progress
	// recording and purging its live KV state.
	removedFromFile := s.streamersFile.RemoveID(id)
	// Also drop any entry keyed on the raw URL.
	if wasURL && s.streamersFile.RemoveID(typed) {
		removedFromFile = true
	}
	removedOneShot := false
	if id.StreamerKey.Valid() && s.kv != nil {
		removedOneShot, _ = vlstate.DeleteOneShot(context.Background(), s.kv, id.StreamerKey)
	}
	if !removedFromFile && !removedOneShot {
		respond(req, OK("not present"))
		return
	}
	if removedFromFile {
		if err := s.streamersFile.Write(); err != nil {
			respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
			return
		}
	}

	// The streamer leaves the subscription, which is also what unsubscribing it
	// cancels an in-progress recording through.
	s.resubscribe()

	respond(req, OK("removed"))
}

// MARK: Record Once
// handleRecordOnce adds a streamer with a monitor=false rule, triggering
// one-shot recording mode. It preflights and takes a variant exactly like an
// ordinary add.
func (s *Service) handleRecordOnce(req micro.Request) {
	var payload AddRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	id, ok := validateStreamerID(req, payload.StreamerRef)
	if !ok {
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	if reason, taken := s.oneShotConflict(id); taken {
		respond(req, fail(ErrConflict, reason))
		return
	}

	id, ok = s.preflight(req, id)
	if !ok {
		return
	}
	// Re-checked on the resolved identity for the reason the add re-checks
	// alreadyAdded: a URL and the platform/username it names look nothing alike
	// until the cloud has decoded it.
	if reason, taken := s.oneShotConflict(id); taken {
		respond(req, fail(ErrConflict, reason))
		return
	}

	if payload.Variant != "" {
		if err := s.streamersFile.SetVariantRule(id, payload.Variant); err != nil {
			respond(req, fail(errInvalidArg, err.Error()))
			return
		}
	}
	if err := s.streamersFile.AddOneShotRule(id); err != nil {
		respond(req, fail(errInvalidArg, err.Error()))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}
	s.markAdded(id)

	// A one-shot is a new tracked streamer, so the subscription changes.
	s.resubscribe()

	respond(req, OK(AddResponse{StreamerRef: id}))
}

// MARK: Monitor/record state rules
//
// A streamer's two switches have three legal pairings:
//
//	monitor on,  record on   — watch forever, record every run
//	monitor on,  record off  — watch forever, record nothing
//	monitor off, record on   — one-shot: record the run happening right now
//
// Monitor off with record off cannot be stored: switching monitoring off moves
// the streamer from streamers.toml into the KV one-shot list, which carries no
// record flag.
//
// A one-shot is only allowed for a live streamer: its KV entry has a short TTL
// that only the start of a recording lifts, and when it expires the streamer is
// gone.
//
// Both rules are enforced here so every client (desktop, CLI, Dashboard) holds
// them.

// isOneShot reports whether the streamer is currently a one-shot, in either
// place one can live: a monitor=false rule in the file it has not been purged
// out of yet, or the KV list it was purged into. The streamers file must
// already be loaded.
func (s *Service) isOneShot(id streamer.StreamerRef) bool {
	if s.streamersFile.HasOneShotRule(id) {
		return true
	}
	if !id.StreamerKey.Valid() || s.kv == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), kvLookupTimeout)
	defer cancel()
	return vlstate.IsOneShot(ctx, s.kv, id.StreamerKey)
}

// monitorOffAllowed reports whether a monitored streamer may be switched to a
// one-shot right now, returning the reason it may not. The streamers file must
// already be loaded.
//
// A streamer whose live status cannot be read is refused. Unlike isRetired,
// where an unreadable document costs a wasted status check, the harmless answer
// here is no: saying yes on a guess can delete the streamer.
func (s *Service) monitorOffAllowed(id streamer.StreamerRef) error {
	if !s.streamersFile.IsRecording(id) {
		return errors.New("recording is off for this streamer — switch recording on before switching monitoring off, or remove the streamer")
	}
	if !id.StreamerKey.Valid() || s.kv == nil {
		return errors.New("cannot tell whether this streamer is live")
	}
	ctx, cancel := context.WithTimeout(context.Background(), kvLookupTimeout)
	defer cancel()
	doc, err := vlstate.GetStreamerDoc(ctx, s.kv, id.StreamerKey)
	if err != nil {
		return errors.New("cannot tell whether this streamer is live")
	}
	// A session already running is the strongest form of "live": it outranks
	// the status field, which lags a stream that just came back.
	if doc.ActiveSessionID != "" {
		return nil
	}
	if doc.Status != streamer.StatusLive && doc.Status != streamer.StatusPaused {
		return fmt.Errorf("only a live streamer can be switched to record-once (this one is %s)", doc.Status)
	}
	return nil
}

// MARK: Disable
// handleRecordOff adds a record=false rule for the given streamer,
// causing the daemon to cancel any in-progress recording and refuse to start
// new ones until the streamer is enabled again. The streamer stays monitored.
func (s *Service) handleRecordOff(req micro.Request) {
	id, ok := parseStreamerID(req)
	if !ok {
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	typed := id
	var wasURL bool
	if id, wasURL, ok = s.resolveURLRef(req, id); !ok {
		return
	}
	if wasURL {
		s.streamersFile.RemoveRecordOffRule(typed)
	}

	if !s.streamersFile.IsRecording(id) {
		respond(req, fail(ErrConflict, "recording already off"))
		return
	}
	// See the monitor/record state rules above: a one-shot cannot hold a
	// record=false rule, and would not be worth keeping if it could.
	if s.isOneShot(id) {
		respond(req, fail(ErrConflict,
			"this streamer is set to record once and isn't monitored — switch monitoring on first, or remove it"))
		return
	}
	if err := s.streamersFile.AddRecordOffRule(id); err != nil {
		respond(req, fail(errInvalidArg, err.Error()))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}

	// The streamer stays subscribed either way — a record=false rule still
	// covers it — so this needs the config applied, not a new subscription.
	s.reload()

	respond(req, OK("recording off"))
}

// MARK: Enable
// handleRecordOn clears any record=false rule covering the given
// streamer, allowing recording to start again on the next status update.
func (s *Service) handleRecordOn(req micro.Request) {
	id, ok := parseStreamerID(req)
	if !ok {
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	typed := id
	var wasURL bool
	if id, wasURL, ok = s.resolveURLRef(req, id); !ok {
		return
	}
	// Clear the resolved rule and, for a URL, any rule keyed on the raw URL.
	// Both calls run: either may be the one holding the streamer off.
	cleared := s.streamersFile.RemoveRecordOffRule(id)
	if wasURL && s.streamersFile.RemoveRecordOffRule(typed) {
		cleared = true
	}
	if !cleared {
		respond(req, OK("already recording"))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}

	s.reload()

	respond(req, OK("recording on"))
}

// MARK: Disable All
// handleRecordOffAll adds a single record=false rule covering every streamer
// still recording, then reloads the config once.
func (s *Service) handleRecordOffAll(req micro.Request) {
	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}
	if !s.streamersFile.RecordOffAll() {
		respond(req, OK("nothing to disable"))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}
	s.reload()
	respond(req, OK("recording off for all"))
}

// MARK: Enable All
// handleRecordOnAll clears record=false from every rule in one write, then
// reloads the config once.
func (s *Service) handleRecordOnAll(req micro.Request) {
	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}
	if !s.streamersFile.RecordOnAll() {
		respond(req, OK("nothing to enable"))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}
	s.reload()
	respond(req, OK("recording on for all"))
}

// MARK: Remove All
// handleRemoveAll clears every streamer from the file in one write, then
// resubscribes once.
func (s *Service) handleRemoveAll(req micro.Request) {
	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}
	if !s.streamersFile.RemoveAll() {
		respond(req, OK("nothing to remove"))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}
	s.resubscribe()
	respond(req, OK("removed all"))
}

// SetVariantRequest is the payload for streamer.set_variant. An empty Variant
// clears any existing variant override for the streamer.
type SetVariantRequest struct {
	streamer.StreamerRef
	Variant string `json:"variant"`
}

// MARK: Set Variant
// handleSetStreamerVariant sets (or clears) the variant override rule for the
// given streamer. The change applies to the next recording; in-progress
// recordings are not affected.
func (s *Service) handleSetStreamerVariant(req micro.Request) {
	var payload SetVariantRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	id, ok := validateStreamerID(req, payload.StreamerRef)
	if !ok {
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	typed := id
	var wasURL bool
	if id, wasURL, ok = s.resolveURLRef(req, id); !ok {
		return
	}
	if wasURL {
		s.streamersFile.RemoveVariantRule(typed)
	}

	if err := s.streamersFile.SetVariantRule(id, payload.Variant); err != nil {
		respond(req, fail(errInvalidArg, err.Error()))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}

	s.reload()

	if payload.Variant == "" {
		respond(req, OK("variant cleared"))
	} else {
		respond(req, OK("variant set"))
	}
}

// setOptionsRequest is the payload for streamer.set_options. It carries the full
// desired state for a streamer's overrides; an inherit-equivalent field (nil,
// true Monitor, or an empty string) clears that override. record is controlled
// by record_off/record_on instead.
type setOptionsRequest struct {
	streamer.StreamerRef
	Monitor         *bool   `json:"monitor"`
	Variant         *string `json:"variant"`
	OutputTemplate  *string `json:"output_template"`
	DeletePartFiles *bool   `json:"delete_part_files"`
	SplitSize       *string `json:"split_size"`
	SplitDuration   *string `json:"split_duration"`
	ArchiveDir      *string `json:"archive_dir"`
}

// MARK: Set Options
// handleSetStreamerOptions consolidates a streamer's per-streamer rule options
// into a single rule. The change applies to the next recording; in-progress
// recordings are not affected.
func (s *Service) handleSetStreamerOptions(req micro.Request) {
	var payload setOptionsRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	id, ok := validateStreamerID(req, payload.StreamerRef)
	if !ok {
		return
	}

	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}

	// Whether this request changes WHICH streamers are tracked. Only a monitor
	// flip does: it moves the streamer between streamers.toml and the KV
	// one-shot list, and the purge that performs the move runs as part of a
	// subscription cycle. Every other field here is a local recording option,
	// which a config reload applies without a subscribe round trip.
	membershipChanged := false

	// Switching monitoring OFF makes the streamer a one-shot, which is only a
	// coherent state for a live streamer that records — see the state rules
	// above. Only the transition is gated: a one-shot editing its variant sends
	// monitor=false unchanged with the rest of the form and must still save.
	if payload.Monitor != nil && !*payload.Monitor && !s.isOneShot(id) {
		if err := s.monitorOffAllowed(id); err != nil {
			respond(req, fail(ErrConflict, err.Error()))
			return
		}
		membershipChanged = true
	}

	if payload.Monitor != nil && *payload.Monitor && vlconfig.NoMonitor(id.StreamerKey.Platform) {
		respond(req, fail(ErrMonitorUnsupported, "Sites without their own support can only be recorded once."))
		return
	}

	// Turning monitoring ON promotes a one-shot back to a monitored streamer. A
	// one-shot is never in [streamers], so move it there from the one-shot list,
	// or it would vanish on the next resubscribe.
	if payload.Monitor != nil && *payload.Monitor {
		wasOneShot := s.streamersFile.HasOneShotRule(id)
		if id.StreamerKey.Valid() {
			if removed, _ := vlstate.DeleteOneShot(context.Background(), s.kv, id.StreamerKey); removed {
				wasOneShot = true
			}
		}
		if wasOneShot {
			s.streamersFile.AddID(id)
			membershipChanged = true
		}
	}

	// Validate split fields before mutating.
	if payload.SplitSize != nil && *payload.SplitSize != "" {
		if _, err := vlconfig.ParseSplitSize(*payload.SplitSize); err != nil {
			respond(req, fail(errInvalidArg, fmt.Sprintf("invalid split_size: %v", err)))
			return
		}
	}
	if payload.SplitDuration != nil && *payload.SplitDuration != "" {
		if _, err := vlconfig.ParseSplitDuration(*payload.SplitDuration); err != nil {
			respond(req, fail(errInvalidArg, fmt.Sprintf("invalid split_duration: %v", err)))
			return
		}
	}
	// Validate a real archive folder (empty/"off" inherit/disable). It must be
	// a valid path that does not overlap the download folder.
	if payload.ArchiveDir != nil && *payload.ArchiveDir != "" && !vlconfig.IsArchiveOff(*payload.ArchiveDir) {
		if err := vlconfig.ValidateArchiveDir(*payload.ArchiveDir); err != nil {
			respond(req, fail(errInvalidArg, fmt.Sprintf("invalid archive_dir: %v", err)))
			return
		}
		if s.configCtl != nil {
			if err := vlconfig.ValidateArchiveDirAgainstDownloadDir(*payload.ArchiveDir, s.configCtl.GetConfig().DownloadDir); err != nil {
				respond(req, fail(errInvalidArg, err.Error()))
				return
			}
		}
		// Reject an override the daemon can't write to up front (an unmounted drive,
		// or in a container a path with no mounted volume behind it).
		if err := CheckArchiveDirWritable(*payload.ArchiveDir); err != nil {
			respond(req, fail(errInvalidArg, err.Error()))
			return
		}
		// An override is a Storage root of its own, so create it here too rather
		// than let it report as unreachable until this streamer's first move.
		if err := vlconfig.EnsureStorageDir(*payload.ArchiveDir); err != nil {
			s.logger.With("func", "handleSetStreamerOptions").
				Warn("Could not create the archive folder",
					"path", *payload.ArchiveDir, "error", err)
		}
	}

	opts := vlconfig.RuleOptions{
		Monitor:         payload.Monitor,
		Variant:         payload.Variant,
		OutputTemplate:  payload.OutputTemplate,
		DeletePartFiles: payload.DeletePartFiles,
		SplitSize:       payload.SplitSize,
		SplitDuration:   payload.SplitDuration,
		ArchiveDir:      payload.ArchiveDir,
	}
	if err := s.streamersFile.SetRuleOptions(id, opts); err != nil {
		respond(req, fail(errInvalidArg, err.Error()))
		return
	}
	if err := s.streamersFile.Write(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to write streamers: %v", err)))
		return
	}

	if membershipChanged {
		s.resubscribe()
	} else {
		s.reload()
	}

	// This request carries the full desired state of the archive override, so it
	// can have just switched the streamer to "off" or cleared the override back
	// onto a global that is off — either way its queued moves may now have
	// nowhere to go. A cleared override arrives as nil, so there is no field to
	// test: re-examine on every call.
	if s.configCtl != nil {
		s.configCtl.ArchiveConfigChanged()
	}

	respond(req, OK("options set"))
}

// MARK: Check
// handleCheckStreamer checks a streamer's status right now, normally by putting
// it at the front of the status queue. An Unavailable or Defunct streamer gets
// the cloud re-check instead, which also clears its retirement; that is the
// only way back from StatusDefunct. The daemon decides, since it owns the
// freshest view docs.
func (s *Service) handleCheckStreamer(req micro.Request) {
	id, ok := parseStreamerID(req)
	if !ok {
		return
	}
	if s.isRetired(id) && s.streamerFuncs.Recheck != nil {
		ctx, cancel := context.WithTimeout(context.Background(), addResolveTimeout)
		defer cancel()
		if err := s.streamerFuncs.Recheck(ctx, id); err != nil {
			respond(req, fail(errInternal, err.Error()))
			return
		}
		respond(req, OK("rechecking"))
		return
	}
	select {
	case s.statusReqPriCh <- id:
		respond(req, OK("checking"))
	default:
		respond(req, fail(errInternal, "status queue full"))
	}
}

// isRetired reports whether a streamer is Unavailable or Defunct, so a check
// must also clear that state. Anything unreadable (a URL-only streamer, no doc
// yet) is treated as not retired, the harmless answer.
func (s *Service) isRetired(id streamer.StreamerRef) bool {
	if !id.StreamerKey.Valid() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), kvLookupTimeout)
	defer cancel()
	doc, err := vlstate.GetStreamerDoc(ctx, s.kv, id.StreamerKey)
	if err != nil {
		return false
	}
	return doc.Status == streamer.StatusUnavailable || doc.Status == streamer.StatusDefunct
}

// streamerListItem is one entry in the streamer.list response: a streamer's
// identity plus its effective per-streamer rule options. Live state (status,
// sessions, stats) is delivered separately via KV; this carries the config that
// is not in KV so a client can show and edit each streamer's options.
type streamerListItem struct {
	Platform string `json:"platform"`
	Username string `json:"username"`
	URL      string `json:"url,omitempty"`
	// DisplayName and Status come from the live view doc rather than
	// streamers.toml, which knows only what is tracked and not what is
	// happening. Status is empty when no doc exists yet (nothing has checked
	// the streamer since it was added).
	DisplayName     string              `json:"display_name,omitempty"`
	Status          streamer.LiveStatus `json:"status,omitempty"`
	OneShot         bool                `json:"one_shot,omitempty"`
	Monitor         bool                `json:"monitor"`
	Record          bool                `json:"record"`
	Variant         string              `json:"variant"`
	OutputTemplate  string              `json:"output_template"`
	DeletePartFiles *bool               `json:"delete_part_files"`
	SplitSize       string              `json:"split_size"`
	SplitDuration   string              `json:"split_duration"`
	ArchiveDir      string              `json:"archive_dir"`
}

// MARK: List
// handleListStreamers returns every tracked streamer with its effective rule
// options. Clients call this on connect and after any mutation to keep their
// per-streamer options in sync with streamers.toml.
func (s *Service) handleListStreamers(req micro.Request) {
	if err := s.streamersFile.Load(); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to load streamers: %v", err)))
		return
	}
	ids := s.streamersFile.AllIDs()

	// A one-shot lives only in the KV list — PurgeOneShots drops it from
	// streamers.toml — so AllIDs alone would report it as untracked while it is
	// being recorded.
	oneShot := map[string]bool{}
	if s.kv != nil {
		for _, id := range vlstate.GetOneShotList(context.Background(), s.kv) {
			if key := id.String(); key != "" && !containsRef(ids, id) {
				ids = append(ids, id)
			}
			oneShot[id.String()] = true
		}
	}

	items := make([]streamerListItem, 0, len(ids))
	for _, id := range ids {
		o := s.streamersFile.OptionsFor(id)
		item := streamerListItem{
			Platform:        id.StreamerKey.Platform,
			Username:        id.StreamerKey.Username,
			URL:             id.URL,
			OneShot:         oneShot[id.String()],
			Monitor:         o.Monitor,
			Record:          o.Record,
			Variant:         o.Variant,
			OutputTemplate:  o.OutputTemplate,
			DeletePartFiles: o.DeletePartFiles,
			SplitSize:       o.SplitSize,
			SplitDuration:   o.SplitDuration,
			ArchiveDir:      o.ArchiveDir,
		}
		if s.kv != nil && id.StreamerKey.Valid() {
			if doc, err := vlstate.GetStreamerDoc(context.Background(), s.kv, id.StreamerKey); err == nil {
				item.Status = doc.Status
				item.DisplayName = doc.DisplayName
				if oneShot[id.String()] {
					// The one-shot list is the authority on monitoring for a
					// streamer the file no longer describes.
					item.Monitor = false
				}
			}
		}
		items = append(items, item)
	}
	respond(req, OK(items))
}

// containsRef reports whether refs already names id.
func containsRef(refs streamer.StreamerRefs, id streamer.StreamerRef) bool {
	for _, r := range refs {
		if r.String() == id.String() {
			return true
		}
	}
	return false
}

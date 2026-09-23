package cloud

import (
	"context"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// The login sweep polls login-gated platforms with the user's stored login. The
// cloud announces status changes it can see, but on a login-gated platform
// every unauthenticated lookup looks the same whether or not anyone is live.
//
// Polling spends a real account's requests, so it is paced by three rules:
//
//  1. a per-streamer interval that widens the longer it has been since the
//     streamer was last seen broadcasting (sweepInterval),
//  2. a tightening to the floor while the streamer is inside the daily window
//     they are usually live in (inBroadcastWindow),
//  3. a per-platform allowance that does NOT scale with how many streamers are
//     tracked, so a daemon watching two hundred emits no more requests than one
//     watching ten (sweepAllowancePerHour).
const (
	// sweepTick is how often the schedule is consulted, not how often anything
	// is checked — the allowance below is what decides that.
	sweepTick = 30 * time.Second

	// sweepFloor is the fastest any one streamer is checked, matching the
	// cloud's default polling interval for an ordinary streamer.
	sweepFloor = 5 * time.Minute
	// sweepCeiling bounds the other end, so a streamer gone for a year is still
	// looked at twice a day and can come back on their own.
	sweepCeiling = 12 * time.Hour
	// sweepUnseen applies to a streamer never yet caught broadcasting: there is
	// no phase to schedule against, so this is a flat, unopinionated rate.
	sweepUnseen = 30 * time.Minute

	// The daily window a streamer is assumed to broadcast in, centred on the
	// last time they were seen live. sweepWindowDefault is used when no duration
	// is known; sweepWindowPad widens a known one at both ends; sweepWindowMax
	// stops one freak all-day broadcast from tightening the rate permanently.
	sweepWindowDefault = 4 * time.Hour
	sweepWindowPad     = 2 * time.Hour
	sweepWindowMax     = 12 * time.Hour

	// sweepAllowancePerHour and sweepAllowanceBurst cap one platform's checks
	// however many streamers are tracked on it: one every three minutes.
	sweepAllowancePerHour = 20.0
	sweepAllowanceBurst   = 4.0

	// sweepUnrunnableHold is how long a streamer is set aside after a check the
	// client could not run at all (e.g. no desktop app to sign it).
	sweepUnrunnableHold = time.Hour
)

// loginSweep holds the sweep's scheduling state: when each streamer is next due
// and what each platform has left to spend. Not persisted; a restart re-checks
// everything once and then settles.
type loginSweep struct {
	mu      sync.Mutex
	entries map[streamer.StreamerKey]*sweepEntry
	budgets map[string]*sweepBudget
	// loginRequired picks the platforms the daemon polls itself: those whose
	// embedded descriptor marks the login required. Tests substitute a stand-in,
	// since no shipped platform sets it.
	loginRequired func(platform string) bool
}

type sweepEntry struct {
	// dueAt is when this streamer may next be checked; lastCheck is what it was
	// computed from, kept so a later stamp can be recognised as later.
	dueAt     time.Time
	lastCheck time.Time
}

func newLoginSweep() *loginSweep {
	return &loginSweep{
		entries:       make(map[streamer.StreamerKey]*sweepEntry),
		budgets:       make(map[string]*sweepBudget),
		loginRequired: vlconfig.LoginRequiredFor,
	}
}

// sweepBudget is a token bucket that refills against the time it is asked with
// rather than a wall-clock ticker, so tests can assert the request rate over a
// simulated day.
type sweepBudget struct {
	tokens   float64
	perHour  float64
	burst    float64
	refilled time.Time
}

func newSweepBudget(now time.Time) *sweepBudget {
	return &sweepBudget{
		tokens:   sweepAllowanceBurst,
		perHour:  sweepAllowancePerHour,
		burst:    sweepAllowanceBurst,
		refilled: now,
	}
}

// take spends one token if the budget has one, refilling first for the time
// elapsed since it was last asked.
func (b *sweepBudget) take(now time.Time) bool {
	if elapsed := now.Sub(b.refilled); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed.Hours()*b.perHour)
		b.refilled = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// noteChecked records that a streamer was checked, pushing its next sweep out.
// Called from the result path, so every authenticated check counts whoever
// asked for it. An earlier stamp never overwrites a later one.
func (s *loginSweep) noteChecked(key streamer.StreamerKey, doc vlstate.StreamerDoc, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	if e == nil {
		e = &sweepEntry{}
		s.entries[key] = e
	}
	if at.Before(e.lastCheck) {
		return
	}
	e.lastCheck = at
	interval := sweepInterval(doc, at)
	e.dueAt = at.Add(interval + jitterFor(interval))
}

// noteUnrunnable sets a streamer aside after a check this machine could not run.
// Retrying it on the ordinary schedule would spend the platform's whole
// allowance on requests that never reach the platform.
func (s *loginSweep) noteUnrunnable(key streamer.StreamerKey, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	if e == nil {
		e = &sweepEntry{}
		s.entries[key] = e
	}
	e.lastCheck = at
	e.dueAt = at.Add(sweepUnrunnableHold)
}

// forget drops a streamer's schedule, so an untracked streamer does not hold
// memory for the life of the daemon.
func (s *loginSweep) forget(key streamer.StreamerKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// due reports whether a streamer may be checked now, and how overdue it is. A
// streamer never seen before is due immediately: it has just been subscribed, or
// the daemon has just started.
func (s *loginSweep) due(key streamer.StreamerKey, now time.Time) (bool, time.Duration) {
	e := s.entries[key]
	if e == nil {
		return true, sweepCeiling
	}
	if now.Before(e.dueAt) {
		return false, 0
	}
	return true, now.Sub(e.dueAt)
}

func (s *loginSweep) budgetFor(platform string, now time.Time) *sweepBudget {
	b := s.budgets[platform]
	if b == nil {
		b = newSweepBudget(now)
		s.budgets[platform] = b
	}
	return b
}

// sweepEligible reports whether a streamer's current state is one the sweep
// should be checking. Not "the doc says restricted": an authed check that finds
// the room offline writes "offline", which would stop the sweep after one pass.
// So it excludes the states where a check has nothing to add.
func sweepEligible(doc vlstate.StreamerDoc) bool {
	if !doc.Monitor && !doc.Record {
		return false // the user turned this streamer off
	}
	if doc.ActiveSessionID != "" {
		return false // already recording; the session reports its own end
	}
	switch doc.Status {
	case streamer.StatusLive, streamer.StatusPaused:
		// Being watched already, by whatever is acting on the live status.
		return false
	case streamer.StatusDefunct, streamer.StatusInvalid, streamer.StatusRemoved:
		// Retired or gone. The sweep never asks for a re-check, so it could not
		// bring one of these back even if it looked.
		return false
	}
	return true
}

// sweepInterval returns how long to wait between checks of one streamer. The
// longer since it was last seen live, the less likely it is to be live soon, so
// the interval widens steeply.
func sweepInterval(doc vlstate.StreamerDoc, now time.Time) time.Duration {
	if inBroadcastWindow(doc, now) {
		return sweepFloor
	}
	if doc.LastLiveStart <= 0 {
		return sweepUnseen
	}
	age := now.Sub(time.Unix(doc.LastLiveStart, 0))
	if age < 0 {
		// A timestamp in the future is a clock that disagrees with ours, not a
		// broadcast that has not happened yet. Treat it as no information.
		return sweepUnseen
	}
	switch {
	case age < 6*time.Hour:
		return 10 * time.Minute
	case age < 24*time.Hour:
		return 20 * time.Minute
	case age < 3*24*time.Hour:
		return 45 * time.Minute
	case age < 7*24*time.Hour:
		return 2 * time.Hour
	case age < 30*24*time.Hour:
		return 6 * time.Hour
	default:
		return sweepCeiling
	}
}

// inBroadcastWindow reports whether now falls in the part of the day this
// streamer is usually live in, assuming a rough daily slot around the time of
// day of LastLiveStart. The window is centred on it because on a login-gated
// platform every check that finds the streamer live re-stamps it, so it may
// fall anywhere within the broadcast.
func inBroadcastWindow(doc vlstate.StreamerDoc, now time.Time) bool {
	if doc.LastLiveStart <= 0 {
		return false
	}
	// Both sides must be read in the SAME location: this compares times of day,
	// and time.Unix yields local time, so a caller working in another zone would
	// otherwise be silently offset by the difference.
	last := time.Unix(doc.LastLiveStart, 0).In(now.Location())
	if now.Before(last) {
		return false // clock skew; no usable phase
	}
	width := time.Duration(sweepWindowDefault)
	if doc.LastLiveDuration > 0 {
		width = time.Duration(doc.LastLiveDuration)*time.Second + sweepWindowPad
	}
	width = min(width, sweepWindowMax)

	const day = 24 * time.Hour
	// Distance between the two times of day, the short way round the clock.
	delta := (timeOfDay(now) - timeOfDay(last) + day) % day
	if delta > day/2 {
		delta = day - delta
	}
	return delta <= width/2
}

func timeOfDay(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
}

// jitterFor spreads checks that would otherwise line up, adding up to a quarter
// of the interval, so a platform sees this machine's requests arrive evenly
// rather than in bursts on the hour.
func jitterFor(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return rand.N(interval / 4)
}

// phaseOffset spreads a streamer's FIRST deadline across its interval, derived
// from its identity so it is the same on every run. A random offset would put
// the whole tracked set back in lockstep after each restart, which is the burst
// this exists to prevent.
func phaseOffset(key streamer.StreamerKey, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key.Platform))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key.Username))
	return time.Duration(h.Sum64() % uint64(interval))
}

// runLoginSweep polls login-gated platforms on the daemon's own schedule until
// ctx is cancelled.
func (c *Client) runLoginSweep(ctx context.Context) {
	log := c.logger.With("func", "runLoginSweep")
	log.Info("Started login sweep")
	defer log.Info("Stopped login sweep")

	ticker := time.NewTicker(sweepTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepOnce(ctx, time.Now())
		}
	}
}

// sweepablePlatforms returns the platforms this daemon should be polling itself:
// login-gated, with a stored login that is currently healthy.
//
// It runs before any doc is read, so a daemon with no logins does no work here.
func (c *Client) sweepablePlatforms() []string {
	if c.cookies == nil {
		return nil
	}
	var out []string
	for _, st := range c.cookies.List() {
		// Status.Healthy folds in the login's expiry hint, and List never
		// exposes a cookie value.
		if !st.Healthy {
			continue
		}
		if !c.sweep.loginRequired(st.Platform) {
			continue
		}
		out = append(out, st.Platform)
	}
	return out
}

// sweepCandidates returns the streamers on one platform that are due a check,
// most overdue first so that nothing starves: a streamer passed over for budget
// keeps accumulating overdue-ness until it wins.
func (c *Client) sweepCandidates(platform string, now time.Time) []streamer.StreamerRef {
	type candidate struct {
		ref     streamer.StreamerRef
		overdue time.Duration
	}
	var cands []candidate

	c.sweep.mu.Lock()
	for _, doc := range c.store.DocsForPlatform(platform) {
		key := streamer.StreamerKey{Platform: doc.Platform, Username: doc.Username}
		if !sweepEligible(doc) {
			continue
		}
		ref := streamer.StreamerRef{StreamerKey: key}
		if !c.IsSubscribed(ref) {
			continue
		}
		ok, overdue := c.sweep.due(key, now)
		if !ok {
			continue
		}
		if e := c.sweep.entries[key]; e == nil {
			// First sighting: stagger it rather than checking the whole tracked
			// set on the tick after startup.
			interval := sweepInterval(doc, now)
			c.sweep.entries[key] = &sweepEntry{dueAt: now.Add(phaseOffset(key, interval))}
			if now.Before(c.sweep.entries[key].dueAt) {
				continue
			}
		}
		cands = append(cands, candidate{ref: ref, overdue: overdue})
	}
	c.sweep.mu.Unlock()

	sort.Slice(cands, func(i, j int) bool { return cands[i].overdue > cands[j].overdue })
	refs := make([]streamer.StreamerRef, len(cands))
	for i, c := range cands {
		refs[i] = c.ref
	}
	return refs
}

// sweepOnce enqueues the checks due at now, and reports how many it sent.
func (c *Client) sweepOnce(ctx context.Context, now time.Time) int {
	if c.sweep == nil {
		return 0
	}
	sent := 0
	for _, platform := range c.sweepablePlatforms() {
		c.sweep.mu.Lock()
		budget := c.sweep.budgetFor(platform, now)
		c.sweep.mu.Unlock()

	platform:
		for _, ref := range c.sweepCandidates(platform, now) {
			c.sweep.mu.Lock()
			allowed := budget.take(now)
			c.sweep.mu.Unlock()
			if !allowed {
				break platform // this platform is spent until it refills
			}
			select {
			case c.authStatusReqCh <- ref:
				// Stamped again from the result path; this is the floor that
				// stops a check which never runs from spinning the sweep.
				c.sweep.noteChecked(ref.StreamerKey, vlstate.StreamerDoc{}, now)
				sent++
			case <-ctx.Done():
				return sent
			default:
				// The queue is full, and draining it is what makes room.
				break platform
			}
		}
	}
	if sent > 0 {
		c.logger.With("func", "sweepOnce").Debug("Login sweep queued checks", "count", sent)
	}
	return sent
}

package cloud

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/zalando/go-keyring"
)

// A fixed reference time. The broadcast-window arithmetic is time-of-day based,
// so tests that care about it pick their own; this one is deliberately mid-day
// and away from a midnight boundary.
var sweepNow = time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)

func docAgedBy(age time.Duration) vlstate.StreamerDoc {
	return vlstate.StreamerDoc{
		Platform:      "bigo",
		Username:      "someone",
		Monitor:       true,
		Status:        streamer.StatusRestricted,
		LastLiveStart: sweepNow.Add(-age).Unix(),
	}
}

// TestSweepIntervalLadder pins the rates a login-gated platform is polled at.
// These numbers are the cost of the feature — someone's real account making
// these requests around the clock — so a change to them should be deliberate
// enough to come here and say so.
func TestSweepIntervalLadder(t *testing.T) {
	// Ages chosen 12h off the reference time so none of them land inside the
	// broadcast window, which is tested on its own below.
	tests := []struct {
		name string
		age  time.Duration
		want time.Duration
	}{
		{"seen a few hours ago", 12*time.Hour + 2*time.Hour, 20 * time.Minute},
		{"seen yesterday", 12*time.Hour + 30*time.Hour, 45 * time.Minute},
		{"seen a few days ago", 12*time.Hour + 5*24*time.Hour, 2 * time.Hour},
		{"seen a fortnight ago", 12*time.Hour + 14*24*time.Hour, 6 * time.Hour},
		{"seen months ago", 12*time.Hour + 90*24*time.Hour, sweepCeiling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sweepInterval(docAgedBy(tt.age), sweepNow); got != tt.want {
				t.Errorf("sweepInterval(age=%s) = %s, want %s", tt.age, got, tt.want)
			}
		})
	}

	// Never caught broadcasting: there is no phase to schedule against, so the
	// rate is flat. The add itself already triggers an immediate check, so this
	// only governs how fast the daemon looks again afterwards.
	unseen := vlstate.StreamerDoc{Platform: "bigo", Username: "nobody", Monitor: true}
	if got := sweepInterval(unseen, sweepNow); got != sweepUnseen {
		t.Errorf("never-seen interval = %s, want %s", got, sweepUnseen)
	}

	// A timestamp in the future is a disagreeing clock, not a broadcast that has
	// not happened yet, and must not be read as "seen a moment ago".
	future := vlstate.StreamerDoc{
		Platform: "bigo", Username: "skewed", Monitor: true,
		LastLiveStart: sweepNow.Add(48 * time.Hour).Unix(),
	}
	if got := sweepInterval(future, sweepNow); got != sweepUnseen {
		t.Errorf("future LastLiveStart interval = %s, want %s", got, sweepUnseen)
	}
}

// TestSweepIntervalStaysInBounds is the guard that matters most for the ladder:
// whatever it is fed, no streamer is ever checked harder than an ordinary
// tracked streamer already is, and none is abandoned entirely.
func TestSweepIntervalStaysInBounds(t *testing.T) {
	ages := []time.Duration{
		0, time.Second, time.Minute, time.Hour, 24 * time.Hour,
		365 * 24 * time.Hour, -time.Hour, -365 * 24 * time.Hour,
	}
	for _, age := range ages {
		for _, dur := range []int64{0, 1, 3600, 86400, -1} {
			doc := docAgedBy(age)
			doc.LastLiveDuration = dur
			got := sweepInterval(doc, sweepNow)
			if got < sweepFloor || got > sweepCeiling {
				t.Errorf("sweepInterval(age=%s, dur=%d) = %s, outside [%s, %s]",
					age, dur, got, sweepFloor, sweepCeiling)
			}
		}
	}
	// Zero-valued doc included: a streamer whose doc has never been filled in
	// must not produce a zero interval, which would be an unbounded hot loop.
	if got := sweepInterval(vlstate.StreamerDoc{}, sweepNow); got < sweepFloor {
		t.Errorf("empty doc interval = %s, want at least %s", got, sweepFloor)
	}
}

// TestInBroadcastWindow covers the schedule tightening. It is the only part of
// the design that uses a streamer's habits, and it does so from a single
// timestamp the doc already carries.
func TestInBroadcastWindow(t *testing.T) {
	// Live at 13:00 with no known duration: a 4h window, so 11:00–15:00.
	doc := vlstate.StreamerDoc{
		Platform: "bigo", Username: "someone", Monitor: true,
		LastLiveStart: sweepNow.Unix(),
	}
	inside := []time.Time{
		sweepNow.AddDate(0, 0, 1),                       // same time tomorrow
		sweepNow.AddDate(0, 0, 3).Add(90 * time.Minute), // 14:30, three days on
		sweepNow.AddDate(0, 0, 1).Add(-90 * time.Minute),
	}
	for _, at := range inside {
		if !inBroadcastWindow(doc, at) {
			t.Errorf("%s should be inside the window", at.Format(time.Kitchen))
		}
		if got := sweepInterval(doc, at); got != sweepFloor {
			t.Errorf("inside the window the interval should be %s, got %s", sweepFloor, got)
		}
	}
	outside := []time.Time{
		sweepNow.AddDate(0, 0, 1).Add(6 * time.Hour),  // 19:00
		sweepNow.AddDate(0, 0, 1).Add(-6 * time.Hour), // 07:00
	}
	for _, at := range outside {
		if inBroadcastWindow(doc, at) {
			t.Errorf("%s should be outside the window", at.Format(time.Kitchen))
		}
	}

	// Midnight wrap: the window is time-of-day arithmetic, so a streamer live at
	// 23:30 must still match 00:30 the next day — a plain subtraction would
	// place them nearly 24h apart.
	lateNight := time.Date(2026, 9, 9, 23, 30, 0, 0, time.UTC)
	late := vlstate.StreamerDoc{
		Platform: "bigo", Username: "nightowl", Monitor: true,
		LastLiveStart: lateNight.Unix(),
	}
	if !inBroadcastWindow(late, lateNight.Add(time.Hour)) { // 00:30
		t.Error("a window centred at 23:30 must contain 00:30")
	}
	// 22:30 the following day — the early side of the window, reached by going
	// forward. A time before the stamp reads as clock skew rather than a window
	// match, which is why this is +23h and not -1h.
	if !inBroadcastWindow(late, lateNight.Add(23*time.Hour)) {
		t.Error("a window centred at 23:30 must contain 22:30")
	}
	if inBroadcastWindow(late, lateNight.Add(8*time.Hour)) { // 07:30
		t.Error("a window centred at 23:30 must not contain 07:30")
	}

	// One freak all-day broadcast must not tighten the rate permanently.
	marathon := vlstate.StreamerDoc{
		Platform: "bigo", Username: "marathon", Monitor: true,
		LastLiveStart: sweepNow.Unix(), LastLiveDuration: int64((72 * time.Hour).Seconds()),
	}
	if inBroadcastWindow(marathon, sweepNow.Add(sweepWindowMax)) {
		t.Errorf("the window must be capped at %s however long the broadcast was", sweepWindowMax)
	}

	// No stamp at all means no window; the flat unseen rate applies instead.
	if inBroadcastWindow(vlstate.StreamerDoc{}, sweepNow) {
		t.Error("a streamer never seen live has no broadcast window")
	}
}

// TestPhaseOffsetIsStable covers the restart property: offsets are derived from
// identity, not chance, so a restart does not put the whole tracked set back
// into lockstep — which is exactly the synchronised burst they prevent.
func TestPhaseOffsetIsStable(t *testing.T) {
	key := streamer.StreamerKey{Platform: "bigo", Username: "someone"}
	first := phaseOffset(key, time.Hour)
	for range 100 {
		if got := phaseOffset(key, time.Hour); got != first {
			t.Fatalf("phaseOffset is not deterministic: %s then %s", first, got)
		}
	}
	if first < 0 || first >= time.Hour {
		t.Errorf("phaseOffset = %s, want within [0, 1h)", first)
	}

	// Different streamers must land on different phases, or staggering achieves
	// nothing. With 200 keys over an hour, collisions should be negligible.
	seen := map[time.Duration]bool{}
	for i := range 200 {
		k := streamer.StreamerKey{Platform: "bigo", Username: fmt.Sprintf("user%d", i)}
		seen[phaseOffset(k, time.Hour)] = true
	}
	if len(seen) < 190 {
		t.Errorf("only %d distinct phases from 200 streamers; offsets are clustering", len(seen))
	}

	// The two halves of the key must not be able to trade characters and
	// produce the same hash ("ab"+"c" vs "a"+"bc").
	a := phaseOffset(streamer.StreamerKey{Platform: "ab", Username: "c"}, time.Hour)
	b := phaseOffset(streamer.StreamerKey{Platform: "a", Username: "bc"}, time.Hour)
	if a == b {
		t.Error("platform and username are not separated in the hash")
	}
}

// TestSweepEligible guards that an offline streamer stays eligible: an authed
// check that finds the room offline writes "offline" into the doc, so a sweep
// that only selected restricted streamers would stop after one pass.
func TestSweepEligible(t *testing.T) {
	base := vlstate.StreamerDoc{Platform: "bigo", Username: "someone", Monitor: true}

	// The load-bearing case.
	offline := base
	offline.Status = streamer.StatusOffline
	if !sweepEligible(offline) {
		t.Error("an offline streamer MUST stay in the sweep: on a login-gated " +
			"platform that is the normal resting state, and dropping it here " +
			"means the sweep checks each streamer exactly once, ever")
	}

	for _, st := range []streamer.LiveStatus{
		streamer.StatusRestricted, streamer.StatusUnknown, streamer.StatusAuthRequired,
	} {
		doc := base
		doc.Status = st
		if !sweepEligible(doc) {
			t.Errorf("status %q should be eligible", st)
		}
	}

	for _, st := range []streamer.LiveStatus{
		streamer.StatusLive, streamer.StatusPaused,
		streamer.StatusDefunct, streamer.StatusInvalid, streamer.StatusRemoved,
	} {
		doc := base
		doc.Status = st
		if sweepEligible(doc) {
			t.Errorf("status %q should not be eligible", st)
		}
	}

	// A recording in flight reports its own end; checking underneath it spends
	// allowance to learn what the session already knows.
	busy := base
	busy.Status = streamer.StatusOffline
	busy.ActiveSessionID = "session-1"
	if sweepEligible(busy) {
		t.Error("a streamer with a session in flight should not be swept")
	}

	// Turned off by the user: neither monitored nor recorded.
	off := base
	off.Status = streamer.StatusOffline
	off.Monitor, off.Record = false, false
	if sweepEligible(off) {
		t.Error("a streamer the user disabled should not be swept")
	}
}

// TestSweepBudgetRateIsBounded asserts the property the whole design rests on,
// against a simulated day: whatever the schedule asks for, one platform's
// checks stay inside its allowance.
func TestSweepBudgetRateIsBounded(t *testing.T) {
	start := sweepNow
	b := newSweepBudget(start)

	spent := 0
	// Ask far more often than the sweep ever would (every 10s for 24h) so the
	// budget, not the caller, is what bounds the result.
	for at := start; at.Before(start.Add(24 * time.Hour)); at = at.Add(10 * time.Second) {
		if b.take(at) {
			spent++
		}
	}
	maxAllowed := int(24*sweepAllowancePerHour + sweepAllowanceBurst)
	if spent > maxAllowed {
		t.Errorf("spent %d checks in a simulated day, allowance is %d", spent, maxAllowed)
	}
	// And it must actually hand tokens out, or the sweep would never check
	// anything and this bound would be trivially satisfied.
	if spent < int(20*sweepAllowancePerHour) {
		t.Errorf("spent only %d checks in a day; the budget is too tight to work", spent)
	}
}

// newSweepTestClient builds a Client with a real doc store and cookie store,
// seeded with n subscribed bigo streamers. No network: the sweep only enqueues
// onto a channel, and this test reads that channel instead of a fetch running.
//
// No shipped platform requires a login, so bigo stands in for one: its login
// descriptor is real, which keeps the session-cookie expiry checks honest.
func newSweepTestClient(t *testing.T, n int, login *platformauth.Session) *Client {
	t.Helper()
	keyring.MockInit() // in-process keyring — never touches the real OS keychain

	origPlatforms := vlconfig.Platforms()
	vlconfig.SetPlatforms(origPlatforms) // restored by the cleanup below
	t.Cleanup(func() { vlconfig.SetPlatforms(origPlatforms) })

	cookies := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	if login != nil {
		if err := cookies.Set(*login); err != nil {
			t.Fatalf("seed cookie store: %v", err)
		}
	}

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	nc, kv := newTestKV(t)
	c := &Client{
		cookies:         cookies,
		kv:              kv,
		store:           vlstate.NewStore(kv),
		logger:          logger,
		localNC:         nc,
		sweep:           newLoginSweep(),
		authStatusReqCh: make(chan streamer.StreamerRef, 4096),
	}
	c.sweep.loginRequired = func(platform string) bool { return platform == "bigo" }

	seeds := make([]vlstate.DocSeed, 0, n)
	refs := make(streamer.StreamerRefs, 0, n)
	for i := range n {
		ref := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{
			Platform: "bigo", Username: fmt.Sprintf("user%d", i),
		}}
		refs = append(refs, ref)
		seeds = append(seeds, vlstate.DocSeed{ID: ref, Opts: vlstate.DocOptions{Monitor: true, Record: true}})
	}
	c.subscriptions.Store(refs)
	c.store.Reconcile(context.Background(), seeds)
	for _, ref := range refs {
		c.store.SetStatus(context.Background(), streamer.StreamerState{
			StreamerRef: ref, Status: streamer.StatusRestricted,
		}, true)
	}
	return c
}

func healthyBigoLogin() *platformauth.Session {
	return &platformauth.Session{
		Platform: "bigo",
		Cookies:  []platformauth.Cookie{{Name: "yyuid", Value: "test"}},
		Healthy:  true,
	}
}

// TestSweepRateDoesNotScaleWithTrackedCount checks that streamers share a fixed
// allowance: a daemon tracking two hundred streamers on a login-gated platform
// makes no more requests than one tracking ten.
func TestSweepRateDoesNotScaleWithTrackedCount(t *testing.T) {
	const span = 12 * time.Hour
	counts := []int{10, 300}
	sent := make(map[int]int, len(counts))
	maxAllowed := int(span.Hours()*sweepAllowancePerHour + sweepAllowanceBurst)

	for _, n := range counts {
		c := newSweepTestClient(t, n, healthyBigoLogin())
		total := 0
		for at := sweepNow; at.Before(sweepNow.Add(span)); at = at.Add(sweepTick) {
			total += c.sweepOnce(context.Background(), at)
		}
		sent[n] = total

		if total > maxAllowed {
			t.Errorf("%d streamers produced %d checks in %s; allowance is %d",
				n, total, span, maxAllowed)
		}
		if total == 0 {
			t.Errorf("%d streamers produced no checks at all", n)
		}
	}

	// The tracked set grew 30x. The request count must not: the streamers share
	// one allowance, so tracking more of them means each is checked less often,
	// not that the platform is asked more. Both totals are already inside the
	// hard cap above; this is the shape of the curve between them.
	if sent[300] > 2*sent[10] {
		t.Errorf("request rate grew with the tracked set: %d streamers sent %d, "+
			"%d streamers sent %d", 10, sent[10], 300, sent[300])
	}
}

// TestSweepIsNoOpWithoutAHealthyLogin checks that the sweep costs nothing at
// all, not a request nor a doc read, unless this daemon holds a usable login
// for a login-gated platform.
func TestSweepIsNoOpWithoutAHealthyLogin(t *testing.T) {
	expired := healthyBigoLogin()
	expired.Cookies = []platformauth.Cookie{
		{Name: "yyuid", Value: "test", Expires: time.Now().Add(-time.Hour).Unix()},
	}

	unhealthy := healthyBigoLogin()
	unhealthy.Healthy = false

	otherPlatform := &platformauth.Session{
		Platform: "twitch",
		Cookies:  []platformauth.Cookie{{Name: "auth-token", Value: "test"}},
		Healthy:  true,
	}

	tests := []struct {
		name  string
		login *platformauth.Session
	}{
		{"no login stored at all", nil},
		{"login for a platform that is not login-gated", otherPlatform},
		{"stored login marked unhealthy", unhealthy},
		{"login whose session cookie has expired", expired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newSweepTestClient(t, 25, tt.login)
			if got := c.sweepOnce(context.Background(), sweepNow); got != 0 {
				t.Errorf("sweepOnce sent %d checks, want 0", got)
			}
			if len(c.authStatusReqCh) != 0 {
				t.Errorf("%d checks queued, want none", len(c.authStatusReqCh))
			}
		})
	}
}

// TestSweepChecksRepeatedly checks that a login-gated streamer, whose status
// the cloud never sees change, keeps being checked.
func TestSweepChecksRepeatedly(t *testing.T) {
	c := newSweepTestClient(t, 1, healthyBigoLogin())

	// A streamer the sweep has not seen before is staggered by its phase offset
	// rather than checked on the spot — the add itself already triggers a check,
	// and starting the whole tracked set at once is the burst the offset exists
	// to prevent. So the first check lands within the first interval, not on the
	// first tick.
	var first int
	firstAt := sweepNow
	for at := sweepNow; at.Before(sweepNow.Add(sweepUnseen + sweepTick)); at = at.Add(sweepTick) {
		if n := c.sweepOnce(context.Background(), at); n > 0 {
			first, firstAt = n, at
			break
		}
	}
	if first == 0 {
		t.Fatalf("no check in the first %s", sweepUnseen)
	}
	// Immediately after, the streamer is not due again.
	if got := c.sweepOnce(context.Background(), firstAt.Add(time.Second)); got != 0 {
		t.Errorf("re-checked a second later: %d", got)
	}
	// But it comes back round.
	var later int
	for at := firstAt; at.Before(firstAt.Add(4 * time.Hour)); at = at.Add(sweepTick) {
		later += c.sweepOnce(context.Background(), at)
	}
	if later < 2 {
		t.Errorf("only %d checks in four hours; the streamer is not being re-checked", later)
	}
}

// TestNoteCheckedIsMonotonic covers the composition with every other path that
// runs an authenticated check. They all stamp through here, and an out-of-order
// stamp (a slow check landing after a fast one) must not drag a deadline back.
func TestNoteCheckedIsMonotonic(t *testing.T) {
	s := newLoginSweep()
	key := streamer.StreamerKey{Platform: "bigo", Username: "someone"}
	doc := docAgedBy(90 * 24 * time.Hour) // the slowest tier, for a wide deadline

	s.noteChecked(key, doc, sweepNow)
	after := s.entries[key].dueAt

	s.noteChecked(key, doc, sweepNow.Add(-time.Hour)) // an older result arriving late
	if s.entries[key].dueAt != after {
		t.Error("a stamp from the past moved the deadline")
	}

	if due, _ := s.due(key, sweepNow.Add(time.Second)); due {
		t.Error("a just-checked streamer should not be due")
	}
	if due, _ := s.due(key, sweepNow.Add(sweepCeiling*2)); !due {
		t.Error("a streamer should always come due eventually")
	}

	// A nil sweep is what every Client built by struct literal in these tests
	// has; the hooks on the fetch path must tolerate it.
	var nilSweep *loginSweep
	nilSweep.noteChecked(key, doc, sweepNow)
	nilSweep.noteUnrunnable(key, sweepNow)
	nilSweep.forget(key)
}

// TestNoteUnrunnableBacksOff covers the headless case: a check that cannot run
// on this machine at all must not be retried on the ordinary schedule, or it
// would spend the platform's whole allowance without a request ever leaving.
func TestNoteUnrunnableBacksOff(t *testing.T) {
	s := newLoginSweep()
	key := streamer.StreamerKey{Platform: "bigo", Username: "someone"}

	s.noteUnrunnable(key, sweepNow)
	if due, _ := s.due(key, sweepNow.Add(sweepUnrunnableHold/2)); due {
		t.Error("an unrunnable check should be held back, not retried promptly")
	}
	if due, _ := s.due(key, sweepNow.Add(sweepUnrunnableHold+time.Minute)); !due {
		t.Error("the hold should expire")
	}
}

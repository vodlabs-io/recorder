package vlconfig

import (
	"regexp"
	"slices"
	"testing"
)

func TestPlatformLoginAllowsHost(t *testing.T) {
	l := &loginFlow{CookieDomains: []string{"tiktok.com"}}
	allow := []string{"tiktok.com", "www.tiktok.com", "webcast.tiktok.com", "WWW.TikTok.com", "www.tiktok.com:443"}
	deny := []string{"eviltiktok.com", "tiktok.com.evil.com", "example.com", "notistiktok.com"}
	for _, h := range allow {
		if !l.AllowsHost(h) {
			t.Errorf("AllowsHost(%q) = false, want true", h)
		}
	}
	for _, h := range deny {
		if l.AllowsHost(h) {
			t.Errorf("AllowsHost(%q) = true, want false", h)
		}
	}
	// A nil descriptor never allows anything.
	var nilLogin *loginFlow
	if nilLogin.AllowsHost("tiktok.com") {
		t.Error("nil loginFlow should not allow any host")
	}
}

func TestDefaultTikTokHasLogin(t *testing.T) {
	// The embedded default must carry TikTok's login descriptor so cookie
	// injection is guarded even before any cloud refresh.
	if l := LoginFlowFor("tiktok"); l == nil {
		t.Fatal("tiktok should have a login descriptor in the embedded default")
	} else if !l.AllowsHost("webcast.tiktok.com") {
		t.Fatalf("tiktok login should allow webcast.tiktok.com, domains=%v", l.CookieDomains)
	}
	if LoginFlowFor("kick") != nil {
		t.Error("kick should not have a login descriptor")
	}
}

// TestDefaultTwitchHasLogin guards the descriptor that lets the Accounts tab
// sign in to Twitch, which is what makes a subscriber-only broadcast recordable.
func TestDefaultTwitchHasLogin(t *testing.T) {
	l := LoginFlowFor("twitch")
	if l == nil {
		t.Fatal("twitch should have a login descriptor in the embedded default")
	}
	// gql.twitch.tv is where the login is actually spent; the apex and www are
	// where it is captured.
	for _, h := range []string{"twitch.tv", "www.twitch.tv", "gql.twitch.tv"} {
		if !l.AllowsHost(h) {
			t.Errorf("twitch login should allow %q, domains=%v", h, l.CookieDomains)
		}
	}
	// usher.ttvnw.net serves the playlist and is authorized by the signed token
	// in its query string, not by the login — so the login has no business
	// going there, nor to any of Twitch's other CDN domains.
	for _, h := range []string{"usher.ttvnw.net", "ttvnw.net", "twitchcdn.net", "nottwitch.tv"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: twitch login must not allow %q", h)
		}
	}
	if l.SessionCookie != "auth-token" {
		t.Errorf("twitch session cookie = %q, want auth-token", l.SessionCookie)
	}
	// Twitch's GraphQL API ignores the Cookie header outright, so without the
	// scheme a stored login authenticates nothing at all.
	if l.Auth == nil {
		t.Fatal("twitch login needs an auth scheme; cookies alone do not authenticate its API")
	}
	if l.Auth.Scheme != "oauth-cookie" || !slices.Equal(l.Auth.Cookies, []string{"auth-token"}) {
		t.Errorf("unexpected twitch auth scheme: %+v", l.Auth)
	}
	// The signing cookie IS the session cookie here: one credential does both
	// jobs, so a capture that stores a login can always authenticate with it.
	if l.Auth.Cookies[0] != l.SessionCookie {
		t.Errorf("twitch auth cookie %q should be its session cookie %q",
			l.Auth.Cookies[0], l.SessionCookie)
	}
	// The refresh reloads a signed-in page rather than the sign-in page, and
	// deliberately not the twitch.tv front page — that one autoplays a stream,
	// and this runs every few minutes for the life of the app.
	if got := l.PageToRefresh(); got != "https://www.twitch.tv/settings/profile" {
		t.Errorf("twitch refresh page = %q, want the account settings page", got)
	}
	// The Accounts row is labelled from this cookie; naming a secret one would
	// publish it to the UI.
	if l.AccountCookie != "login" {
		t.Errorf("twitch account cookie = %q, want login", l.AccountCookie)
	}
}

// TestDefaultYouTubeHasLogin guards the descriptor that lets the Accounts tab
// sign in to YouTube: the cookie allowlist must cover the hosts a check talks
// to, and must NOT cover google.com — the sign-in page's own domain, whose
// cookies belong to the whole Google account rather than to YouTube.
func TestDefaultYouTubeHasLogin(t *testing.T) {
	l := LoginFlowFor("youtube")
	if l == nil {
		t.Fatal("youtube should have a login descriptor in the embedded default")
	}
	for _, h := range []string{"youtube.com", "www.youtube.com", "m.youtube.com"} {
		if !l.AllowsHost(h) {
			t.Errorf("youtube login should allow %q, domains=%v", h, l.CookieDomains)
		}
	}
	// The jar is captured on youtube.com, which carries its own copies of the
	// account cookies. Nothing needs (or may have) the wider google.com set.
	for _, h := range []string{"accounts.google.com", "google.com", "notyoutube.com"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: youtube login must not allow %q", h)
		}
	}
	// LOGIN_INFO is the one cookie that distinguishes a YouTube session from a
	// bare Google one; capture keys off it, so a rename here breaks sign-in.
	if l.SessionCookie != "LOGIN_INFO" {
		t.Errorf("youtube session cookie = %q, want LOGIN_INFO", l.SessionCookie)
	}
	// The background refresh must reload youtube.com, not the Google sign-in
	// page it signs in through — that page is hit every few minutes.
	if got := l.PageToRefresh(); got != "https://www.youtube.com/" {
		t.Errorf("youtube refresh page = %q, want the youtube.com home page", got)
	}
	// InnerTube refuses a cookie-only request, so the jar alone unlocks nothing.
	if l.Auth == nil {
		t.Fatal("youtube login needs an auth scheme; cookies alone do not authenticate InnerTube")
	}
	if l.Auth.Scheme != "sapisidhash" || l.Auth.Origin != "https://www.youtube.com" {
		t.Errorf("unexpected youtube auth scheme: %+v", l.Auth)
	}
	// Both spellings, preferred first: the jar carries whichever the account has.
	if len(l.Auth.Cookies) == 0 || l.Auth.Cookies[0] != "__Secure-3PAPISID" {
		t.Errorf("youtube auth cookies = %v, want __Secure-3PAPISID first", l.Auth.Cookies)
	}
	// The signing cookie must be one the capture actually keeps, or the login
	// stores clean and then silently fails to authenticate.
	for _, name := range l.Auth.Cookies {
		if name == "" {
			t.Error("empty auth cookie name")
		}
	}
}

// TestDefaultBigoHasLogin guards the descriptor BIGO needs to be recordable: a
// room lookup without a login rarely comes back with the stream URL, so a
// stored login is what records it.
func TestDefaultBigoHasLogin(t *testing.T) {
	l := LoginFlowFor("bigo")
	if l == nil {
		t.Fatal("bigo should have a login descriptor in the embedded default")
	}
	// ta.bigo.tv answers the room lookup the login is spent on; www is where it
	// is captured.
	for _, h := range []string{"bigo.tv", "www.bigo.tv", "ta.bigo.tv"} {
		if !l.AllowsHost(h) {
			t.Errorf("bigo login should allow %q, domains=%v", h, l.CookieDomains)
		}
	}
	// The media hosts are authorized by the signed URL the lookup returns, not
	// by the login, so the jar has no business reaching them.
	for _, h := range []string{"cubetecn.com", "bigovideo.tv", "notbigo.tv"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: bigo login must not allow %q", h)
		}
	}
	// yyuid is the signed-in account id and is absent for anonymous visitors,
	// unlike the language and consent cookies bigo.tv sets on first paint. Drop
	// it and capture fires on those instead, storing a jar that is not a login.
	if l.SessionCookie != "yyuid" {
		t.Errorf("bigo session cookie = %q, want yyuid", l.SessionCookie)
	}
	// yyuid is a numeric id, not a name — labelling the Accounts row with it
	// would publish a bare id and tell the user nothing about which account.
	if l.AccountCookie != "" {
		t.Errorf("bigo account cookie = %q, want none", l.AccountCookie)
	}
	// The refresh reloads a signed-in page, not the sign-in page.
	if got := l.PageToRefresh(); got != "https://www.bigo.tv/" {
		t.Errorf("bigo refresh page = %q, want the bigo.tv home page", got)
	}
	// Cookies alone authenticate the room lookup; there is nothing to sign.
	if l.Auth != nil {
		t.Errorf("bigo needs no auth scheme, got %+v", l.Auth)
	}
	// Platforms whose plain checks still see live and offline, with the login
	// needed only for some streams — for bigo, the stream URL. Marking one of
	// these required would put every user's real account on a 24/7 poll.
	for _, p := range []string{"bigo", "tiktok", "twitch", "youtube", "kick"} {
		if LoginRequiredFor(p) {
			t.Errorf("%s must not be login-required: its plain checks still see live and offline", p)
		}
	}
}

// TestSetPlatformsLoginRequiredIsClientAuthoritative extends the guard on the
// rest of the login descriptor to the flag that decides how often a user's own
// account is spent.
func TestSetPlatformsLoginRequiredIsClientAuthoritative(t *testing.T) {
	snapshotPlatforms(t)

	// No shipped platform requires a login, so embed a stand-in that does.
	embeddedLogins["walled"] = &loginFlow{
		URL: "https://walled.example/login", CookieDomains: []string{"walled.example"}, Required: true,
	}
	t.Cleanup(func() { delete(embeddedLogins, "walled") })

	SetPlatforms([]Platform{
		// A hostile list tries to switch it on for a platform whose login is
		// real but whose lookups are not gated...
		{Name: "twitch", Login: &loginFlow{
			URL: "https://www.twitch.tv/login", CookieDomains: []string{"twitch.tv"},
			Required: true,
		}},
		// ...to invent one for a platform that has no login at all...
		{Name: "kick", Login: &loginFlow{
			URL: "https://kick.com/", CookieDomains: []string{"kick.com"}, Required: true,
		}},
		// ...and to switch it OFF where it belongs, which would quietly stop the
		// daemon polling the one platform nothing else can observe.
		{Name: "walled", Login: &loginFlow{
			URL: "https://walled.example/login", CookieDomains: []string{"walled.example"}, Required: false,
		}},
	})

	if LoginRequiredFor("twitch") {
		t.Fatal("SECURITY: backend turned twitch into a login-required platform")
	}
	if LoginFlowFor("kick") != nil {
		t.Fatal("SECURITY: backend gave a login to a platform that has none")
	}
	// The embedded truth survives the hostile list. (A platform the list omits
	// is simply absent; what the list must not do is rewrite a descriptor.)
	if !LoginRequiredFor("walled") {
		t.Error("SECURITY: backend cleared an embedded Required flag")
	}
}

// TestPlatformEqualNoticesRequired guards SetPlatforms' change detection, which
// compares login descriptors field by field and would silently ignore a new one.
func TestPlatformEqualNoticesRequired(t *testing.T) {
	a := Platform{Name: "p", Login: &loginFlow{URL: "u"}}
	b := Platform{Name: "p", Login: &loginFlow{URL: "u", Required: true}}
	if platformEqual(a, b) {
		t.Error("platformEqual ignores Required; a change to it would not propagate")
	}
}

// TestSetPlatformsAuthSchemeIsClientAuthoritative extends the CookieDomains
// guard to the auth scheme: it decides what gets computed from a user's
// credentials and where it is sent, so the cloud must not be able to introduce
// or redirect one.
func TestSetPlatformsAuthSchemeIsClientAuthoritative(t *testing.T) {
	snapshotPlatforms(t)

	SetPlatforms([]Platform{
		// A hostile list tries to bind YouTube's signature to its own origin...
		{Name: "youtube", Login: &loginFlow{
			URL: "https://accounts.google.com/", CookieDomains: []string{"youtube.com"},
			Auth: &authScheme{Scheme: "sapisidhash", Cookies: []string{"SAPISID"},
				Origin: "https://attacker.example"},
		}},
		// ...and to give a platform that has no login one that signs to it.
		{Name: "kick", Login: &loginFlow{
			URL: "https://kick.com/", CookieDomains: []string{"kick.com"},
			Auth: &authScheme{Scheme: "sapisidhash", Cookies: []string{"session"},
				Origin: "https://attacker.example"},
		}},
	})

	yt := LoginFlowFor("youtube")
	if yt == nil || yt.Auth == nil {
		t.Fatal("youtube login or auth scheme unexpectedly nil")
	}
	if yt.Auth.Origin != "https://www.youtube.com" {
		t.Fatalf("SECURITY: backend redirected the signature origin to %q", yt.Auth.Origin)
	}
	if LoginFlowFor("kick") != nil {
		t.Fatal("SECURITY: backend gave a login (and an auth scheme) to a platform that has none")
	}
}

func TestPageToRefreshFallsBackToLoginURL(t *testing.T) {
	// A platform that signs in on its own site needs no separate refresh page.
	if got := LoginFlowFor("tiktok").PageToRefresh(); got != LoginFlowFor("tiktok").URL {
		t.Errorf("tiktok refresh page = %q, want its login URL", got)
	}
	var nilLogin *loginFlow
	if got := nilLogin.PageToRefresh(); got != "" {
		t.Errorf("nil loginFlow refresh page = %q, want empty", got)
	}
}

// TestDefaultTiLiveHasLogin guards the descriptor behind TiLive's fallback: a
// broadcast the service cannot see is checked with the user's own login, which
// TiLive's API accepts only as signed headers.
func TestDefaultTiLiveHasLogin(t *testing.T) {
	l := LoginFlowFor("tilive")
	if l == nil {
		t.Fatal("tilive should have a login descriptor in the embedded default")
	}
	// app.ti.live is where the login is captured, api.ti.live where it is spent.
	for _, h := range []string{"app.ti.live", "api.ti.live"} {
		if !l.AllowsHost(h) {
			t.Errorf("tilive login should allow %q, domains=%v", h, l.CookieDomains)
		}
	}
	// The stream CDN is authorized by its signed URL alone.
	for _, h := range []string{"pull.yah.live", "txlivecdn.com", "netease.im"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: tilive login must not allow %q", h)
		}
	}
	if l.Required {
		t.Error("tilive is polled by the service; the login is a fallback, not required")
	}
	if l.Auth == nil || l.Auth.Scheme != "tilive-checksum" ||
		!slices.Equal(l.Auth.Cookies, []string{"token", "userId"}) {
		t.Errorf("unexpected tilive auth scheme: %+v", l.Auth)
	}
	if l.SessionCookie != l.Auth.Cookies[0] {
		t.Errorf("tilive session cookie %q should be the token the scheme signs with", l.SessionCookie)
	}
}

// TestDefaultXHasLogin guards the descriptor behind X's fallback: a check the
// service cannot make is made with the user's own login, which X's web API
// accepts only with ct0 repeated as a CSRF header.
func TestDefaultXHasLogin(t *testing.T) {
	l := LoginFlowFor("x")
	if l == nil {
		t.Fatal("x should have a login descriptor in the embedded default")
	}
	for _, h := range []string{"x.com", "api.x.com"} {
		if !l.AllowsHost(h) {
			t.Errorf("x login should allow %q, domains=%v", h, l.CookieDomains)
		}
	}
	// The video CDNs need no login.
	for _, h := range []string{"prod-fastly-us-east-1.video.pscp.tv", "pbs.twimg.com", "x.com.evil.test"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: x login must not allow %q", h)
		}
	}
	if l.Required {
		t.Error("x is polled by the service; the login is a fallback, not required")
	}
	if l.Auth == nil || l.Auth.Scheme != "x-csrf" || !slices.Equal(l.Auth.Cookies, []string{"ct0"}) {
		t.Errorf("unexpected x auth scheme: %+v", l.Auth)
	}
	if !regexp.MustCompile(l.SuccessURLPattern).MatchString("https://x.com/home") {
		t.Errorf("success pattern %q misses the home timeline", l.SuccessURLPattern)
	}
}

// TestDefaultFanslyHasLogin guards the descriptor behind Fansly's locked
// streams: the session token lives in the page's local storage, so the desktop
// keeps it in a cookie of the jar, and the scheme replays it as the
// Authorization header the API reads.
func TestDefaultFanslyHasLogin(t *testing.T) {
	l := LoginFlowFor("fansly")
	if l == nil {
		t.Fatal("fansly should have a login descriptor in the embedded default")
	}
	for _, h := range []string{"fansly.com", "apiv3.fansly.com"} {
		if !l.AllowsHost(h) {
			t.Errorf("fansly login should allow %q, domains=%v", h, l.CookieDomains)
		}
	}
	// The IVS playlist and segments are authorized by their signed URLs alone.
	for _, h := range []string{"live-video.net", "8e69c661e048.us-east-1.playback.live-video.net", "cdn3.fansly.com.evil.test"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: fansly login must not allow %q", h)
		}
	}
	if len(l.StorageCookies) != 1 || l.StorageCookies[0] != (storageCookie{Key: "session_active_session", Field: "token", Cookie: "vl_session_token"}) {
		t.Errorf("unexpected fansly storage cookies: %+v", l.StorageCookies)
	}
	if l.Auth == nil || l.Auth.Scheme != "authorization-cookie" || !slices.Equal(l.Auth.Cookies, []string{"vl_session_token"}) {
		t.Errorf("unexpected fansly auth scheme: %+v", l.Auth)
	}
	if l.SessionCookie != "vl_session_token" {
		t.Errorf("fansly session cookie %q should be the copied token", l.SessionCookie)
	}
}

// TestDefaultCameraPriveHasLogin guards the descriptor behind CameraPrive's
// member seats: the room page alone is read with the login's cookies, which
// the media and chat servers on livecamcdn.com never see.
func TestDefaultCameraPriveHasLogin(t *testing.T) {
	l := LoginFlowFor("cameraprive")
	if l == nil {
		t.Fatal("cameraprive should have a login descriptor in the embedded default")
	}
	if !l.AllowsHost("cameraprive.com") || !l.AllowsHost("www.cameraprive.com") {
		t.Errorf("cameraprive login should allow the site, domains=%v", l.CookieDomains)
	}
	for _, h := range []string{"livecamcdn.com", "tuna.livecamcdn.com", "chat2.livecamcdn.com", "cameraprive.com.evil.test"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: cameraprive login must not allow %q", h)
		}
	}
	if l.Auth != nil {
		t.Errorf("cameraprive needs no auth scheme: %+v", l.Auth)
	}
	if l.SessionCookie != "remember-login-user" {
		t.Errorf("cameraprive session cookie %q", l.SessionCookie)
	}
}

func TestDefaultJoystickHasLogin(t *testing.T) {
	l := LoginFlowFor("joystick")
	if l == nil {
		t.Fatal("joystick should have a login descriptor in the embedded default")
	}
	if !l.AllowsHost("joystick.tv") || !l.AllowsHost("api.joystick.tv") {
		t.Errorf("joystick login should allow the site, domains=%v", l.CookieDomains)
	}
	// The playback CDN is authorized by the token in the stream URL, not by the
	// login, so the login's cookies have no business going there.
	for _, h := range []string{"cdn.playbackhost.com", "playbackhost.com", "joystick.tv.evil.test"} {
		if l.AllowsHost(h) {
			t.Errorf("SECURITY: joystick login must not allow %q", h)
		}
	}
	// No auth scheme: the seat reads the token out of the "user" cookie itself.
	if l.Auth != nil {
		t.Errorf("joystick needs no auth scheme: %+v", l.Auth)
	}
	if l.SessionCookie != "user" {
		t.Errorf("joystick session cookie %q", l.SessionCookie)
	}
}

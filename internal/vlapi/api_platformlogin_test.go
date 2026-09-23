package vlapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/zalando/go-keyring"

	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/platformlogin"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// newTestNATSKV spins up an embedded JetStream server with a "streamers" KV
// bucket — the same setup the daemon uses — so the account vlapi flow can be
// driven over real NATS.
func newTestNATSKV(t *testing.T) (*nats.Conn, jetstream.KeyValue) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "streamers"})
	if err != nil {
		t.Fatalf("create kv: %v", err)
	}
	return nc, kv
}

// TestAccountLoginRejectsJarWithoutSessionCookie covers a Cookie header pasted
// from the wrong tab: a google.com paste carries a real Google session but no
// LOGIN_INFO, so it would store as "Active" and unlock nothing.
func TestAccountLoginRejectsJarWithoutSessionCookie(t *testing.T) {
	keyring.MockInit()

	nc, kv := newTestNATSKV(t)
	store := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	if _, err := Init(
		nc, vlconfig.AppConfig{}, nil, nil, nil, nil, logger,
		testSignals(), make(chan streamer.StreamerRef, 4),
		kv, nil, StreamerFuncs{}, store, nil,
	); err != nil {
		t.Fatalf("vlapi init: %v", err)
	}

	// A plausible-looking Google jar with no LOGIN_INFO in it.
	_, err = Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform:     "youtube",
		CookieHeader: "SID=abc; HSID=def; SAPISID=ghi",
	})
	if err == nil {
		t.Fatal("a jar with no session cookie was accepted as a login")
	}
	if !strings.Contains(err.Error(), "LOGIN_INFO") {
		t.Errorf("error should name the missing cookie, got: %v", err)
	}
	if len(store.List()) != 0 {
		t.Errorf("rejected login must not be stored, got %+v", store.List())
	}

	// The same jar with the session cookie present is a login.
	res, err := Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform:     "youtube",
		CookieHeader: "SID=abc; LOGIN_INFO=xyz; SAPISID=ghi",
	})
	if err != nil {
		t.Fatalf("set with session cookie: %v", err)
	}
	if res.Platform != "youtube" || res.CookieCount != 3 || !res.Healthy {
		t.Fatalf("unexpected set status: %+v", res)
	}
}

// TestAccountLoginLabelsTheAccount covers the Accounts row's Account column,
// which matters because what a login unlocks is per-account.
func TestAccountLoginLabelsTheAccount(t *testing.T) {
	keyring.MockInit()

	nc, kv := newTestNATSKV(t)
	store := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	if _, err := Init(
		nc, vlconfig.AppConfig{}, nil, nil, nil, nil, logger,
		testSignals(), make(chan streamer.StreamerRef, 4),
		kv, nil, StreamerFuncs{}, store, nil,
	); err != nil {
		t.Fatalf("vlapi init: %v", err)
	}

	// Twitch's descriptor names "login" as the cookie carrying the account name.
	res, err := Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform:     "twitch",
		CookieHeader: "auth-token=secretvalue; login=someviewer; unique_id=abc",
	})
	if err != nil {
		t.Fatalf("set twitch login: %v", err)
	}
	if res.Account != "someviewer" {
		t.Errorf("account label = %q, want someviewer", res.Account)
	}

	// A caller that supplies its own label keeps it.
	res, err = Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform:     "twitch",
		Account:      "chosen",
		CookieHeader: "auth-token=secretvalue; login=someviewer",
	})
	if err != nil {
		t.Fatalf("set twitch login with account: %v", err)
	}
	if res.Account != "chosen" {
		t.Errorf("account label = %q, want the supplied one", res.Account)
	}

	// A platform whose descriptor names no account cookie gets no label rather
	// than an arbitrary cookie's value.
	res, err = Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform:     "tiktok",
		CookieHeader: "sessionid=abc; msToken=def",
	})
	if err != nil {
		t.Fatalf("set tiktok login: %v", err)
	}
	if res.Account != "" {
		t.Errorf("account label = %q, want empty for a platform with no account cookie", res.Account)
	}
}

// TestAccountLoginFlow drives platformlogin.set/list/remove over real NATS and
// asserts the PRIVACY invariant: a stored cookie's secret value never appears in
// the account KV doc (only non-secret status does).
func TestAccountLoginFlow(t *testing.T) {
	keyring.MockInit() // in-process keyring — never touches the real OS keychain

	nc, kv := newTestNATSKV(t)
	store := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	statusReqPriCh := make(chan streamer.StreamerRef, 4)
	if _, err := Init(
		nc, vlconfig.AppConfig{}, nil, nil, nil, nil, logger,
		testSignals(), statusReqPriCh,
		kv, nil, StreamerFuncs{}, store, nil,
	); err != nil {
		t.Fatalf("vlapi init: %v", err)
	}

	// Pre-seed streamer view docs: one tiktok streamer stuck at a login wall
	// (must be re-checked after sign-in), one already live (must be left alone).
	seedDoc := func(platform, username string, status streamer.LiveStatus) streamer.StreamerKey {
		t.Helper()
		key := streamer.StreamerKey{Platform: platform, Username: username}
		doc, err := json.Marshal(vlstate.StreamerDoc{
			Schema: vlstate.DocSchema, Platform: platform, Username: username, Status: status,
		})
		if err != nil {
			t.Fatalf("marshal doc: %v", err)
		}
		if _, err := kv.Put(context.Background(), vlstate.DocKVKey(key), doc); err != nil {
			t.Fatalf("seed doc: %v", err)
		}
		return key
	}
	restrictedKey := seedDoc("tiktok", "walled", streamer.StatusRestricted)
	seedDoc("tiktok", "openstreamer", streamer.StatusLive)

	const secret = "SUPERSECRET_SESSIONID_ABC123"

	// set — the advanced paste path.
	setRes, err := Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform:     "tiktok",
		CookieHeader: "sessionid=" + secret + "; sid_tt=deadbeef",
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if setRes.CookieCount != 2 || !setRes.Healthy || setRes.Platform != "tiktok" {
		t.Fatalf("unexpected set status: %+v", setRes)
	}

	// Signing in must nudge exactly the tracked streamers stuck at a login wall
	// (a re-check that escalates to an authed one) — not the already-live one.
	select {
	case id := <-statusReqPriCh:
		if id.StreamerKey != restrictedKey {
			t.Fatalf("nudged wrong streamer: %v, want %v", id.StreamerKey, restrictedKey)
		}
	default:
		t.Fatal("sign-in did not nudge the restricted streamer for a re-check")
	}
	select {
	case id := <-statusReqPriCh:
		t.Fatalf("unexpected extra nudge: %v (live streamers must be left alone)", id)
	default:
	}

	// PRIVACY: the account KV doc must carry only non-secret status.
	entry, err := kv.Get(context.Background(), platformlogin.Key("tiktok"))
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	doc := string(entry.Value())
	if strings.Contains(doc, secret) || strings.Contains(doc, "deadbeef") {
		t.Fatalf("SECRET COOKIE VALUE LEAKED into account KV doc: %s", doc)
	}
	if !strings.Contains(doc, "cookie_count") {
		t.Fatalf("account doc missing status fields: %s", doc)
	}

	// list — returns non-secret status; must not carry cookie values either.
	listRes, err := Request[any, []platformauth.Status](nc, "platformlogin.list", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(*listRes) != 1 || (*listRes)[0].Platform != "tiktok" {
		t.Fatalf("unexpected list: %+v", *listRes)
	}

	// remove — deletes the login and its account doc.
	if _, err := Request[removeLoginRequest, bool](nc, "platformlogin.remove", removeLoginRequest{Platform: "tiktok"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := kv.Get(context.Background(), platformlogin.Key("tiktok")); err == nil {
		t.Fatal("account KV doc should be deleted after remove")
	}
	if store.Has("tiktok") {
		t.Fatal("login should be gone from the store after remove")
	}
}

// TestAccountLoginHealth drives platformlogin.health — the desktop background
// refresh's "the platform ended the session" report — and asserts it flips both
// the stored login and the Accounts KV doc to unhealthy.
func TestAccountLoginHealth(t *testing.T) {
	keyring.MockInit()
	nc, kv := newTestNATSKV(t)
	store := platformauth.NewStore(platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")))
	logger, _ := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if _, err := Init(nc, vlconfig.AppConfig{}, nil, nil, nil, nil, logger,
		testSignals(), make(chan streamer.StreamerRef, 1), kv, nil, StreamerFuncs{}, store, nil); err != nil {
		t.Fatalf("vlapi init: %v", err)
	}

	if _, err := Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform: "tiktok", CookieHeader: "sessionid=abc",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	if _, err := Request[setLoginHealthRequest, bool](nc, "platformlogin.health", setLoginHealthRequest{
		Platform: "tiktok", Healthy: false,
	}); err != nil {
		t.Fatalf("health: %v", err)
	}

	sess, ok := store.Get("tiktok")
	if !ok {
		t.Fatal("login should still exist after a health flip (only remove deletes it)")
	}
	if st := sess.Status(); st.Healthy {
		t.Fatalf("login should be unhealthy after the health report: %+v", st)
	}

	entry, err := kv.Get(context.Background(), platformlogin.Key("tiktok"))
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	var st platformauth.Status
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		t.Fatalf("unmarshal account doc: %v", err)
	}
	if st.Healthy {
		t.Fatalf("account KV doc should show unhealthy: %s", entry.Value())
	}
}

// TestAccountLoginRejectsUnknownPlatform confirms a platform with no login
// descriptor can't have cookies stored.
func TestAccountLoginRejectsUnknownPlatform(t *testing.T) {
	keyring.MockInit()
	nc, kv := newTestNATSKV(t)
	store := platformauth.NewStore(platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")))
	logger, _ := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if _, err := Init(nc, vlconfig.AppConfig{}, nil, nil, nil, nil, logger,
		testSignals(), make(chan streamer.StreamerRef, 1), kv, nil, StreamerFuncs{}, store, nil); err != nil {
		t.Fatalf("vlapi init: %v", err)
	}

	// kick has no login descriptor in the default platform list.
	_, err := Request[setLoginRequest, platformauth.Status](nc, "platformlogin.set", setLoginRequest{
		Platform: "kick", CookieHeader: "auth=abc",
	})
	if err == nil {
		t.Fatal("expected set to be rejected for a platform without login support")
	}
}

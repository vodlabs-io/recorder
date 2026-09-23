package platformauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return &Store{
		filePath:    filepath.Join(t.TempDir(), "cookies.json"),
		keyOverride: key,
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := testStore(t)

	sess := Session{
		Platform:   "tiktok",
		Account:    "@someone",
		CapturedAt: 1000,
		Cookies: []Cookie{
			{Name: "sessionid", Value: "secret", HTTPOnly: true, Expires: 5000},
			{Name: "sid_tt", Value: "secret2", Expires: 4000},
		},
	}
	if err := s.Set(sess); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok := s.Get("tiktok")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if got.Header() != "sessionid=secret; sid_tt=secret2" {
		t.Fatalf("Header() = %q", got.Header())
	}
	// tiktok's login descriptor designates "sessionid" as the session cookie,
	// so the hint tracks ITS expiry (5000) — not the earlier sid_tt (4000).
	// Short-lived auxiliary cookies must not drive the countdown.
	if got.ExpiresHint() != 5000 {
		t.Fatalf("ExpiresHint() = %d, want 5000 (session cookie)", got.ExpiresHint())
	}
	if !s.Has("tiktok") {
		t.Fatal("Has should be true")
	}
}

// TestStatusExpiredByHint proves the non-secret Status projection reports a
// login unhealthy once its session cookie's expiry has passed — the only
// "Expired" driver for platforms with no reliable rejected-login check signal.
func TestStatusExpiredByHint(t *testing.T) {
	past := Session{
		Platform: "tiktok",
		Healthy:  true,
		Cookies:  []Cookie{{Name: "sessionid", Value: "x", Expires: 1000}}, // long past
	}
	if past.Status().Healthy {
		t.Fatal("session with passed expiry hint should report unhealthy")
	}

	future := Session{
		Platform: "tiktok",
		Healthy:  true,
		Cookies:  []Cookie{{Name: "sessionid", Value: "x", Expires: 4102444800}}, // year 2100
	}
	if !future.Status().Healthy {
		t.Fatal("session with future expiry hint should stay healthy")
	}

	// A session flagged unhealthy by a real check stays unhealthy regardless.
	rejected := future
	rejected.Healthy = false
	if rejected.Status().Healthy {
		t.Fatal("rejected session must not be revived by a future hint")
	}
}

func TestStoreEncryptedAtRest(t *testing.T) {
	s := testStore(t)
	if err := s.Set(Session{Platform: "tiktok", Cookies: []Cookie{{Name: "sessionid", Value: "TOPSECRET"}}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(raw), "TOPSECRET") || strings.Contains(string(raw), "sessionid") {
		t.Fatalf("cookie material found in plaintext on disk: %s", raw)
	}
	// File must be 0600.
	info, err := os.Stat(s.filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}
}

func TestStoreListIsNonSecret(t *testing.T) {
	s := testStore(t)
	_ = s.Set(Session{Platform: "tiktok", Account: "@a", CapturedAt: 1, Cookies: []Cookie{{Name: "sessionid", Value: "x"}}})
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	if list[0].CookieCount != 1 || list[0].Account != "@a" {
		t.Fatalf("unexpected status: %+v", list[0])
	}
}

func TestStoreRemoveDeletesFile(t *testing.T) {
	s := testStore(t)
	_ = s.Set(Session{Platform: "tiktok", Cookies: []Cookie{{Name: "sessionid", Value: "x"}}})
	if err := s.Remove("tiktok"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s.Has("tiktok") {
		t.Fatal("session should be gone")
	}
	if _, err := os.Stat(s.filePath); !os.IsNotExist(err) {
		t.Fatalf("empty store should leave no file, stat err = %v", err)
	}
}

func TestParseCookieHeader(t *testing.T) {
	cases := []struct {
		in   string
		want string // Header() output
	}{
		{"sessionid=abc; sid_tt=def", "sessionid=abc; sid_tt=def"},
		{"Cookie: sessionid=abc; sid_tt=def", "sessionid=abc; sid_tt=def"},
		{"  sessionid = abc ;; sid_tt=def ", "sessionid=abc; sid_tt=def"},
		{"sessionid=abc; sessionid=dup", "sessionid=abc"}, // de-dupe by name
		{"", ""},
	}
	for _, c := range cases {
		sess := Session{Cookies: ParseCookieHeader(c.in)}
		if got := sess.Header(); got != c.want {
			t.Fatalf("ParseCookieHeader(%q) → Header()=%q, want %q", c.in, got, c.want)
		}
	}
}

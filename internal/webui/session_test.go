package webui

import (
	"strings"
	"testing"
	"time"
)

var (
	testSecret = []byte("0123456789abcdef0123456789abcdef")
	testToken  = strings.Repeat("t", 43)
)

func TestCodecRoundTrip(t *testing.T) {
	c := newCodec(testSecret, testToken)
	now := time.Unix(1_800_000_000, 0)
	cl, err := newClaims(now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.decode(c.encode(cl), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.sid != cl.sid || !got.iat.Equal(cl.iat) || !got.exp.Equal(cl.exp) {
		t.Errorf("round trip = %+v, want %+v", got, cl)
	}
	if !got.exp.Equal(now.Add(sessionTTL)) {
		t.Errorf("exp = %v, want 30 days on", got.exp)
	}
}

func TestCodecRejects(t *testing.T) {
	c := newCodec(testSecret, testToken)
	now := time.Unix(1_800_000_000, 0)
	cl, _ := newClaims(now)
	v := c.encode(cl)

	flip := []byte(v)
	flip[3] ^= 1
	cases := map[string]struct {
		codec codec
		value string
		at    time.Time
	}{
		"tampered":       {c, string(flip), now},
		"expired":        {c, v, now.Add(sessionTTL)},
		"no tag":         {c, strings.Split(v, ".")[0], now},
		"rotated token":  {newCodec(testSecret, testToken+"x"), v, now},
		"rotated secret": {newCodec([]byte("another secret, 32 bytes long!!!"), testToken), v, now},
	}
	for name, tc := range cases {
		if _, err := tc.codec.decode(tc.value, tc.at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Someone who knows only the token can't mint a cookie: the key also needs
// the session secret, which never leaves the config folder.
func TestTokenAloneCannotForge(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cl, _ := newClaims(now)
	for _, guess := range [][]byte{nil, []byte(testToken), make([]byte, 32)} {
		forged := newCodec(guess, testToken).encode(cl)
		if _, err := newCodec(testSecret, testToken).decode(forged, now); err == nil {
			t.Errorf("forged with secret %q", guess)
		}
	}
}

func TestRevocationsPersistAndPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	r := loadRevocations(dir)
	old, _ := newClaims(now)
	if err := r.revoke(old.sid, now); err != nil {
		t.Fatal(err)
	}
	if !loadRevocations(dir).revoked(old.sid) {
		t.Fatal("revocation not persisted")
	}
	later := now.Add(sessionTTL + time.Second)
	fresh, _ := newClaims(later)
	if err := r.revoke(fresh.sid, later); err != nil {
		t.Fatal(err)
	}
	reloaded := loadRevocations(dir)
	if reloaded.revoked(old.sid) {
		t.Error("expired revocation not pruned")
	}
	if !reloaded.revoked(fresh.sid) {
		t.Error("fresh revocation lost")
	}
}

func TestReadSecretCreatesOnce(t *testing.T) {
	dir := t.TempDir()
	a, err := readSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := readSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || len(a) != 32 {
		t.Error("secret not stable")
	}
}

package cloud

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

// TestSignRequestSignsAFreshNonce covers that every signed request carries its
// own nonce, and that the nonce is part of what the signature covers, so it
// cannot be swapped or stripped without breaking the signature.
func TestSignRequestSignsAFreshNonce(t *testing.T) {
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	pub, _ := kp.PublicKey()
	c := reregisterTestClient(t)
	c.nkeySeed = string(seed)

	sign := func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, "https://api.example/api/v1/streamers/?a=1", nil)
		req.Header.Set("X-Timestamp", "1700000000")
		req.Header.Set("X-Client-Version", "0.1.0")
		req.Header.Set("X-Client-ID", pub)
		if err := c.signRequest(req, nil); err != nil {
			t.Fatalf("signRequest: %v", err)
		}
		return req
	}
	verify := func(req *http.Request) error {
		body := sha256.Sum256(nil)
		signed := strings.Join([]string{
			req.Method, req.URL.Path, req.URL.Query().Encode(), hex.EncodeToString(body[:]),
			req.Header.Get("Content-Type"), req.Header.Get("X-Timestamp"),
			req.Header.Get("X-Client-Version"), req.Header.Get("X-Client-ID"),
			req.Header.Get("X-Nonce"),
		}, "\n")
		sig, _ := base64.RawURLEncoding.DecodeString(req.Header.Get("X-Signature"))
		return kp.Verify([]byte(signed), sig)
	}

	a, b := sign(), sign()
	if a.Header.Get("X-Nonce") == "" || a.Header.Get("X-Nonce") == b.Header.Get("X-Nonce") {
		t.Fatalf("nonces %q and %q: want two distinct ones", a.Header.Get("X-Nonce"), b.Header.Get("X-Nonce"))
	}
	if err := verify(a); err != nil {
		t.Fatalf("signature does not cover the nonce: %v", err)
	}
	a.Header.Set("X-Nonce", b.Header.Get("X-Nonce"))
	if verify(a) == nil {
		t.Fatal("a swapped nonce still verified")
	}
}

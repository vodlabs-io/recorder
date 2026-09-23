package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/zalando/go-keyring"
)

// TestRegisterGeneratesItsOwnKey covers that registration sends only the
// public half of a key made on this device, with a valid proof that this
// client holds it, and keeps the private half locally.
func TestRegisterGeneratesItsOwnKey(t *testing.T) {
	keyring.MockInit()
	const nonce = "challenge-nonce"
	var sent []string // every header value the service received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case registerChallengeEndpoint:
			json.NewEncoder(w).Encode(challengeResponse{Nonce: nonce, Difficulty: 0})
		case registerEndpoint:
			pub := r.Header.Get("X-NKey-Public")
			kp, err := nkeys.FromPublicKey(pub)
			if err != nil {
				http.Error(w, "bad key", http.StatusBadRequest)
				return
			}
			sig, _ := base64.RawURLEncoding.DecodeString(r.Header.Get("X-NKey-Signature"))
			if kp.Verify([]byte(registerKeyProofPrefix+nonce), sig) != nil {
				http.Error(w, "bad proof", http.StatusBadRequest)
				return
			}
			for _, vs := range r.Header {
				sent = append(sent, vs...)
			}
			json.NewEncoder(w).Encode(map[string]string{"client_id": pub, "account_id": "ATEST"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	c := reregisterTestClient(t)
	c.httpClient = srv.Client()
	if err := c.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, v := range sent {
		if strings.Contains(v, c.nkeySeed) {
			t.Fatal("the seed left this device")
		}
	}
	kp, err := nkeys.FromSeed([]byte(c.nkeySeed))
	if err != nil {
		t.Fatalf("stored seed: %v", err)
	}
	if pub, _ := kp.PublicKey(); pub != c.clientID {
		t.Fatalf("stored seed is for %s, client registered as %s", pub, c.clientID)
	}
}

// TestRegisterRejectsAnotherKey covers a service answering with an identity
// other than the key this client generated: it holds no seed for that one.
func TestRegisterRejectsAnotherKey(t *testing.T) {
	keyring.MockInit()
	other, _ := nkeys.CreateUser()
	otherPub, _ := other.PublicKey()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registerChallengeEndpoint {
			json.NewEncoder(w).Encode(challengeResponse{Nonce: "n", Difficulty: 0})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"client_id": otherPub, "account_id": "ATEST"})
	}))
	defer srv.Close()
	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	c := reregisterTestClient(t)
	c.httpClient = srv.Client()
	if err := c.register(context.Background()); err == nil {
		t.Fatal("register accepted an identity this client holds no key for")
	}
}

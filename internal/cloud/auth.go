package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nats-io/nkeys"
	"github.com/zalando/go-keyring"
)

const (
	// API endpoints
	registerEndpoint          = "/api/v1/auth/register"
	registerChallengeEndpoint = "/api/v1/auth/register/challenge"
	subscribeEndpoint         = "/api/v1/auth/subscribe"

	// Keyring configuration
	keyringService   = "vodlabs"
	keyringNKeySeed  = "nkey_seed"
	keyringClientID  = "client_id"
	keyringAccountID = "account_id"
)

// endpointHost returns the host:port of the active API endpoint when it
// differs from prod, or "" for the prod default.
func endpointHost() string {
	if apiBaseURL == prodAPIBaseURL {
		return ""
	}
	if u, err := url.Parse(apiBaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return apiBaseURL
}

// keyringServiceName namespaces stored credentials by cloud endpoint, so a
// daemon pointed at a non-prod backend (VLREC_CLOUD_API_URL) registers and
// keeps its own identity without touching the prod credentials.
func keyringServiceName() string {
	if h := endpointHost(); h != "" {
		return keyringService + "@" + h
	}
	return keyringService
}

// credsFilePath returns the path to the credentials file in the user's home
// directory, namespaced by endpoint when a non-prod backend is active.
func credsFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	if h := endpointHost(); h != "" {
		return filepath.Join(home, ".vodlabs-creds-"+strings.ReplaceAll(h, ":", "-")+".json")
	}
	return filepath.Join(home, ".vodlabs-creds.json")
}

// fileCreds is the structure for file-based credential storage
type fileCreds struct {
	NKeySeed  string `json:"nkey_seed"`
	ClientID  string `json:"client_id"`
	AccountID string `json:"account_id"`
}

// loadCredentials loads stored credentials from keyring, falling back to file
func (c *Client) loadCredentials() error {
	log := c.logger.With("func", "loadCredentials")
	seed, err := keyring.Get(keyringServiceName(), keyringNKeySeed)
	if err == nil {
		clientID, err := keyring.Get(keyringServiceName(), keyringClientID)
		if err == nil {
			accountID, err := keyring.Get(keyringServiceName(), keyringAccountID)
			if err == nil {
				c.nkeySeed = seed
				c.clientID = clientID
				c.accountID = accountID
				log.Info("Loaded credentials from keyring",
					"client_id", clientID,
					"account_id", accountID)
				return nil
			}
		}
	}

	data, err := os.ReadFile(credsFilePath())
	if err != nil {
		return fmt.Errorf("no stored credentials: %w", err)
	}

	var creds fileCreds
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("failed to parse credentials file: %w", err)
	}

	c.nkeySeed = creds.NKeySeed
	c.clientID = creds.ClientID
	c.accountID = creds.AccountID
	log.Info("Loaded credentials from file", "client_id", c.clientID, "account_id", c.accountID)
	return nil
}

// saveCredentials saves credentials to keyring, falling back to file
func (c *Client) saveCredentials() error {
	log := c.logger.With("func", "saveCredentials", "client_id", c.clientID, "account_id", c.accountID)
	keyringErr := keyring.Set(keyringServiceName(), keyringNKeySeed, c.nkeySeed)
	if keyringErr == nil {
		keyringErr = keyring.Set(keyringServiceName(), keyringClientID, c.clientID)
	}
	if keyringErr == nil {
		keyringErr = keyring.Set(keyringServiceName(), keyringAccountID, c.accountID)
	}

	if keyringErr == nil {
		log.Info("Saved credentials to keyring")
		return nil
	}

	log.Info("Keyring unavailable, saving credentials to file", "error", keyringErr)
	creds := fileCreds{
		NKeySeed:  c.nkeySeed,
		ClientID:  c.clientID,
		AccountID: c.accountID,
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}
	if err := os.WriteFile(credsFilePath(), data, 0600); err != nil {
		return fmt.Errorf("failed to write credentials file: %w", err)
	}
	log.Info("Saved credentials to file", "path", credsFilePath())
	return nil
}

// registerResponse is the response for GET /api/v1/auth/register
type registerResponse struct {
	ClientID  string `json:"client_id"`
	AccountID string `json:"account_id"`
}

// registerKeyProofPrefix starts the message register signs with the new key:
// this prefix, then the challenge nonce. The service checks the same text.
const registerKeyProofPrefix = "vodlabs-register\n"

// challengeResponse is the response for GET /api/v1/auth/register/challenge.
type challengeResponse struct {
	Nonce      string `json:"nonce"`
	Difficulty int    `json:"difficulty"` // required leading zero bits
}

// powSolveBudget bounds how many attempts solvePoW will try. The service asks
// for a small difficulty, which takes a small fraction of this; the cap only
// makes solvePoW fail fast if the service ever asks for far more.
const powSolveBudget = 200_000_000

// fetchPoWChallenge asks the service for a proof-of-work nonce to solve
// before /register will accept a new account. Splitting this from register
// is what makes the work real: a solution can't be computed before the
// nonce it's bound to exists.
func (c *Client) fetchPoWChallenge(ctx context.Context) (challengeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+registerChallengeEndpoint, nil)
	if err != nil {
		return challengeResponse{}, fmt.Errorf("failed to create request: %w", err)
	}
	c.setClientHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return challengeResponse{}, fmt.Errorf("challenge request failed: %w", err)
	}
	defer resp.Body.Close()
	if c.noteBlockedReply(ctx, resp) {
		return challengeResponse{}, errBlocked
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return challengeResponse{}, fmt.Errorf("challenge request failed: %s - %s", resp.Status, string(body))
	}

	var result challengeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return challengeResponse{}, fmt.Errorf("failed to parse challenge response: %w", err)
	}
	return result, nil
}

// solvePoW brute-forces a solution such that sha256(nonce + ":" + solution)
// has at least `difficulty` leading zero bits, counted from the most
// significant bit of the first byte. The service checks exactly this.
func solvePoW(nonce string, difficulty int) (string, error) {
	for i := 0; i < powSolveBudget; i++ {
		solution := strconv.Itoa(i)
		if powMeetsDifficulty(nonce, solution, difficulty) {
			return solution, nil
		}
	}
	return "", fmt.Errorf("no proof-of-work solution found within %d attempts", powSolveBudget)
}

func powMeetsDifficulty(nonce, solution string, difficulty int) bool {
	sum := sha256.Sum256([]byte(nonce + ":" + solution))
	for i := 0; i < difficulty; i++ {
		byteIdx, bitIdx := i/8, 7-i%8
		if byteIdx >= len(sum) {
			return false
		}
		if sum[byteIdx]&(1<<uint(bitIdx)) != 0 {
			return false
		}
	}
	return true
}

// register registers a new client with the API.
//
// The key that identifies this client is generated here, and its private half
// never leaves this device: the service receives the public key and a
// signature over the challenge nonce, proving this client holds it.
func (c *Client) register(ctx context.Context) error {
	log := c.logger.With("func", "register")

	challenge, err := c.fetchPoWChallenge(ctx)
	if err != nil {
		return fmt.Errorf("failed to get registration challenge: %w", err)
	}
	solution, err := solvePoW(challenge.Nonce, challenge.Difficulty)
	if err != nil {
		return fmt.Errorf("failed to solve registration challenge: %w", err)
	}

	userKey, err := nkeys.CreateUser()
	if err != nil {
		return fmt.Errorf("failed to create client key: %w", err)
	}
	defer userKey.Wipe()
	seed, err := userKey.Seed()
	if err != nil {
		return fmt.Errorf("failed to read client key: %w", err)
	}
	publicKey, err := userKey.PublicKey()
	if err != nil {
		return fmt.Errorf("failed to read client key: %w", err)
	}
	proof, err := userKey.Sign([]byte(registerKeyProofPrefix + challenge.Nonce))
	if err != nil {
		return fmt.Errorf("failed to sign registration: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+registerEndpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setClientHeaders(req)
	req.Header.Set("X-PoW-Nonce", challenge.Nonce)
	req.Header.Set("X-PoW-Solution", solution)
	req.Header.Set("X-NKey-Public", publicKey)
	req.Header.Set("X-NKey-Signature", base64.RawURLEncoding.EncodeToString(proof))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("registration request failed: %w", err)
	}
	defer resp.Body.Close()
	if c.noteBlockedReply(ctx, resp) {
		return errBlocked
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registration failed: %s - %s", resp.Status, string(body))
	}

	var result registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if result.ClientID != publicKey {
		return fmt.Errorf("registration returned client %q, not the key this client generated", result.ClientID)
	}

	c.nkeySeed = string(seed)
	c.clientID = result.ClientID
	c.accountID = result.AccountID

	log.Info("Registered new client", "client_id", c.clientID, "account_id", c.accountID)

	if err := c.saveCredentials(); err != nil {
		return err
	}

	return nil
}

// signRequest signs req with this client's key. It also gives req a random
// X-Nonce, signed with the rest, which the service accepts only once, so a
// copy of the request cannot be sent again.
func (c *Client) signRequest(req *http.Request, bodyBytes []byte) error {
	bodyHash := sha256.Sum256(bodyBytes)
	bodyHashHex := hex.EncodeToString(bodyHash[:])

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("failed to create request nonce: %w", err)
	}
	req.Header.Set("X-Nonce", base64.RawURLEncoding.EncodeToString(nonce))

	lines := []string{
		req.Method,
		req.URL.Path,
		req.URL.Query().Encode(),
		bodyHashHex,
		req.Header.Get("Content-Type"),
		req.Header.Get("X-Timestamp"),
		req.Header.Get("X-Client-Version"),
		req.Header.Get("X-Client-ID"),
		req.Header.Get("X-Nonce"),
	}
	stringToSign := strings.Join(lines, "\n")

	userKey, err := nkeys.FromSeed([]byte(c.nkeySeed))
	if err != nil {
		return fmt.Errorf("invalid nkey seed: %w", err)
	}
	defer userKey.Wipe()

	sig, err := userKey.Sign([]byte(stringToSign))
	if err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	encodedSig := base64.RawURLEncoding.EncodeToString(sig)
	req.Header.Set("X-Signature", encodedSig)

	return nil
}

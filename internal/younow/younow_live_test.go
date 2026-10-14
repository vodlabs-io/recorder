//go:build live

package younow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOpen_Network records YOUNOW_USER's live broadcast for YOUNOW_SECONDS
// (default 30) into YOUNOW_OUT (an FLV file) or nowhere. The video token is
// minted with the signed-in session in YOUNOW_LOGIN_FILE, a JSON file
// {"session": "<PHPSESSID>"}.
func TestOpen_Network(t *testing.T) {
	user, file := os.Getenv("YOUNOW_USER"), os.Getenv("YOUNOW_LOGIN_FILE")
	if user == "" || file == "" {
		t.Skip("YOUNOW_USER and YOUNOW_LOGIN_FILE not set")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(raw, &login); err != nil || login.Session == "" {
		t.Fatalf("login file: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.younow.com/php/api/broadcast/info/curId=0/lang=en/user="+user, nil)
	req.Header.Set("Cookie", "PHPSESSID="+login.Session)
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		ErrorCode      int             `json:"errorCode"`
		BroadcastID    json.RawMessage `json:"broadcastId"`
		VideoAuthToken string          `json:"videoAuthToken"`
	}
	err = json.NewDecoder(resp.Body).Decode(&in)
	resp.Body.Close()
	if err != nil || in.ErrorCode != 0 || in.VideoAuthToken == "" {
		t.Fatalf("broadcast/info: error %d, token %v, %v", in.ErrorCode, in.VideoAuthToken != "", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(in.VideoAuthToken, ".")[1])
	var claims struct {
		UserID string `json:"userId"`
	}
	_ = json.Unmarshal(payload, &claims)
	r := Room{
		Broadcast: strings.Trim(string(in.BroadcastID), `"`),
		Token:     in.VideoAuthToken,
		User:      claims.UserID,
		Signaling: "wss://signaling.younow-prod.video.propsproject.com",
	}

	secs, _ := strconv.Atoi(os.Getenv("YOUNOW_SECONDS"))
	if secs <= 0 {
		secs = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stream, err := Open(ctx, r.URL(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var out io.Writer = io.Discard
	if p := os.Getenv("YOUNOW_OUT"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out = f
	}
	n, err := io.Copy(out, stream)
	t.Logf("read %d bytes (%v)", n, err)
	if n < 100_000 {
		t.Errorf("only %d bytes in %d s", n, secs)
	}
}

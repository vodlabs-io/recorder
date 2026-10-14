package asiancammodels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBroadcastURLRoundTrip(t *testing.T) {
	b := Broadcast{Room: "Sweet_Vixen", Chatroom: "10505861", Stream: "c24ad750d65315ebd48820aa263cf651"}
	got, err := Parse(b.URL())
	if err != nil || got != b {
		t.Fatalf("Parse(%q) = %+v, %v", b.URL(), got, err)
	}
	if !IsURL(b.URL()) || IsURL("https://www.asiancammodels.com/x/webcam/") {
		t.Error("IsURL misjudged a URL")
	}
	for _, bad := range []string{
		"asiancammodels://trinity?chatroom=10505781",
		"asiancammodels://trinity?chatroom=x&stream=c24ad750d65315ebd48820aa263cf651",
		"asiancammodels://?chatroom=1&stream=c24ad750d65315ebd48820aa263cf651",
		"livejasmin://trinity?chatroom=1&stream=c24ad750d65315ebd48820aa263cf651",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func stubSite(t *testing.T, body string, check func(*http.Request)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := siteOrigin
	siteOrigin = srv.URL
	t.Cleanup(func() { siteOrigin = old })
}

func TestFetchGrant(t *testing.T) {
	b := Broadcast{Room: "trinity", Chatroom: "10505781", Stream: "c24ad750d65315ebd48820aa263cf651"}
	stubSite(t, `{"data":"{}","token":"215961441070511242127867","ws_url":"wss:\/\/sf2.asiancammodels.com:5443\/WebRTCAppEE\/websocket","subscriber":{"id":"mc:guest_name:0:df99","code":"310272"}}`,
		func(r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/xhr/get-stream-token" || q.Get("chatroom_id") != b.Chatroom || q.Get("stream_hash") != b.Stream ||
				r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				t.Errorf("grant request %s %v", r.URL, r.Header)
			}
		})
	g, err := fetchGrant(context.Background(), http.DefaultClient, b)
	if err != nil {
		t.Fatal(err)
	}
	if g.WebSocketURL != "wss://sf2.asiancammodels.com:5443/WebRTCAppEE/websocket" || g.StreamID != b.Stream ||
		g.Token != "215961441070511242127867" || g.SubscriberID != "mc:guest_name:0:df99" || g.SubscriberCode != "310272" {
		t.Errorf("grant %+v", g)
	}
}

func TestFetchGrantRefusesForeignServers(t *testing.T) {
	b := Broadcast{Room: "trinity", Chatroom: "10505781", Stream: "c24ad750d65315ebd48820aa263cf651"}
	for _, body := range []string{
		`{"token":"1","ws_url":"wss:\/\/evil.example.com\/websocket","subscriber":{"id":"a","code":"1"}}`,
		`{"error":"CHATROOM:NOT_FOUND"}`,
		`{"error":404}`,
	} {
		stubSite(t, body, nil)
		if g, err := fetchGrant(context.Background(), http.DefaultClient, b); err == nil {
			t.Errorf("%s: grant %+v accepted", body, g)
		} else if strings.Contains(body, "error") && !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v", body, err)
		}
	}
}

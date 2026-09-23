package cloud

import (
	"net/http"
	"testing"
)

// A platform's Set-Cookie is minted for this machine's address, so it must
// never ride back with the answer to a check.
func TestReplyHeadersDropsSetCookie(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Add("Set-Cookie", "sessionid=secret; Path=/")
	h.Add("Set-Cookie", "tt_chain_token=also-secret")
	h.Set("Server", "nginx")
	h.Set("X-Anti-Bot", "token-for-this-ip")

	got := replyHeaders(h, false)

	if _, ok := got["Set-Cookie"]; ok {
		t.Fatal("reply carried Set-Cookie")
	}
	for key := range got {
		switch key {
		case "Content-Type", "Content-Length", "Location":
		default:
			t.Fatalf("reply carried an unexpected header: %s", key)
		}
	}
	if got["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q, want the platform's value", got["Content-Type"])
	}
}

func TestReplyHeadersMarksExtraction(t *testing.T) {
	if got := replyHeaders(http.Header{}, true); got["X-Scrape-Extracted"] != "true" {
		t.Fatalf("X-Scrape-Extracted = %q, want true", got["X-Scrape-Extracted"])
	}
	if got := replyHeaders(http.Header{}, false); len(got) != 0 {
		t.Fatalf("headers = %v, want none", got)
	}
}

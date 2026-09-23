package cloud

import (
	"strings"
	"testing"
)

// A login webview renders a page FOR THE SIGNED-IN VIEWER, so their own account
// sits beside whatever was asked about. Only the named fields may leave.
func TestNarrowBrowserBodyDropsViewerState(t *testing.T) {
	page := `{"LiveRoom":{"liveRoomUserInfo":{"user":{"roomId":"7"},"liveRoom":{"status":2}}},` +
		`"AppContext":{"user":{"uid":"viewer-1","email":"viewer@example.com"}}}`

	got, err := narrowBrowserBody(page, "LiveRoom.liveRoomUserInfo")
	if err != nil {
		t.Fatalf("narrowBrowserBody: %v", err)
	}
	for _, leak := range []string{"viewer-1", "viewer@example.com", "AppContext"} {
		if strings.Contains(got, leak) {
			t.Fatalf("narrowed body still carries %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, `"roomId":"7"`) {
		t.Fatalf("narrowed body lost the room it was asked for: %s", got)
	}
}

func TestNarrowBrowserBodyRefusesWithoutAPath(t *testing.T) {
	if _, err := narrowBrowserBody(`{"anything":1}`, ""); err == nil {
		t.Fatal("a request naming no fields was answered with the page")
	}
}

func TestNarrowBrowserBodyRefusesAMissingPath(t *testing.T) {
	if _, err := narrowBrowserBody(`{"LiveRoom":{}}`, "LiveRoom.liveRoomUserInfo"); err == nil {
		t.Fatal("a body without the named fields was returned anyway")
	}
}

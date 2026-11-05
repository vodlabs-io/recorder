package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// A 424 is the service saying this machine's own request to the platform
// failed. It is told apart from other failures, and carries the service's words.
func TestStatusCheckFailedHere(t *testing.T) {
	const body = "This recorder could not reach Twitch: connection reset by peer."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, http.StatusFailedDependency)
	}))
	defer srv.Close()
	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	c, _ := blockTestClient(t, &blockServer{Server: srv})
	id := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "someone"}}
	_, err := c.fetchStatus(context.Background(), statusOpts{}, id)

	var unreachable *PlatformUnreachableError
	if !errors.As(err, &unreachable) || unreachable.Error() != body {
		t.Fatalf("error = %v, want the service's words as a PlatformUnreachableError", err)
	}
	if errors.Is(err, ErrCheckNotRunnable) {
		t.Fatal("a failed request is not a check the service could not run")
	}
}

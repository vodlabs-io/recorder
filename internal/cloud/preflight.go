package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

const resolveEndpoint = "/api/v1/streamers/resolve"

// A rejectionError is the cloud declining to accept a streamer, with a message
// written for the person who typed it. codeUnavailable is also a
// rejectionError: an add that could not be checked is refused rather than
// kept, since status comes only from the cloud.
type rejectionError struct {
	Code     string
	Message  string
	Platform string
	// cause is the transport failure behind an unavailable verdict, kept out of
	// Message because the user can't act on it.
	cause error
}

func (e *rejectionError) Error() string { return e.Message }

func (e *rejectionError) Unwrap() error { return e.cause }

// Definitive reports whether the cloud judged the streamer itself, as opposed
// to being unable to answer. It is the shape vlapi reads this error through, so
// it can classify a rejection without importing this package.
func (e *rejectionError) Definitive() bool { return e.Code != codeUnavailable }

// UnsupportedSite reports that no platform claims the URL, which may still be
// a page this machine can record once itself.
func (e *rejectionError) UnsupportedSite() bool { return e.Code == codeUnsupportedSite }

// resolveResponse is the accepted answer: the canonical identity to store.
type resolveResponse struct {
	Platform string `json:"platform"`
	Username string `json:"username"`
	URL      string `json:"url,omitempty"`
}

// ResolveStreamer asks the cloud to name a streamer and confirm it exists,
// returning the canonical platform/username to store. It is the preflight for
// an add and runs before anything is written, so a URL that names no streamer
// never becomes an entry. Every failure, including a network failure, is a
// *rejectionError.
func (c *Client) ResolveStreamer(ctx context.Context, id streamer.StreamerRef) (streamer.StreamerRef, error) {
	if c.isBlocked() {
		return streamer.StreamerRef{}, blockedRejection()
	}
	body, err := json.Marshal(id)
	if err != nil {
		return streamer.StreamerRef{}, &rejectionError{Code: codeUnavailable, Message: err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBaseURL+resolveEndpoint, bytes.NewReader(body))
	if err != nil {
		return streamer.StreamerRef{}, &rejectionError{Code: codeUnavailable, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	c.setClientHeaders(req)
	req.Header.Set("X-Client-ID", c.clientID)
	if err := c.signRequest(req, body); err != nil {
		return streamer.StreamerRef{}, &rejectionError{Code: codeUnavailable, Message: err.Error()}
	}

	// Only the caller's deadline bounds a preflight: the shared client's
	// shorter ceiling would pre-empt the cloud's own answer about which platform
	// failed and why.
	client := &http.Client{Transport: c.httpClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return streamer.StreamerRef{}, unreachable(ctx, err)
	}
	defer resp.Body.Close()
	if c.noteBlockedReply(ctx, resp) {
		return streamer.StreamerRef{}, blockedRejection()
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return streamer.StreamerRef{}, unreachable(ctx, err)
	}

	if resp.StatusCode != http.StatusOK {
		var rejection struct {
			Error    string `json:"error"`
			Message  string `json:"message"`
			Platform string `json:"platform"`
		}
		if json.Unmarshal(raw, &rejection) == nil && rejection.Error != "" {
			return streamer.StreamerRef{}, &rejectionError{
				Code:     rejection.Error,
				Message:  rejection.Message,
				Platform: rejection.Platform,
			}
		}
		// An unparseable body means something between here and the API answered
		// — a proxy, a gateway, a rate limiter. Not a verdict on the streamer.
		return streamer.StreamerRef{}, &rejectionError{
			Code:    codeUnavailable,
			Message: fmt.Sprintf("The cloud could not check that right now (%s).", resp.Status),
		}
	}

	var out resolveResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return streamer.StreamerRef{}, unreachable(ctx, err)
	}
	if out.Platform == "" || out.Username == "" {
		return streamer.StreamerRef{}, &rejectionError{
			Code:    codeUnavailable,
			Message: "The cloud accepted that streamer but did not name it.",
		}
	}

	// Key form only, which is what a resolved entry looks like everywhere else
	// in streamers.toml. The URL has done its job by now.
	return streamer.StreamerRef{
		StreamerKey: streamer.StreamerKey{Platform: out.Platform, Username: out.Username},
	}, nil
}

// unreachable wraps a transport-level failure as a rejection that does not
// blame the streamer, telling a timeout (retry as-is) apart from an unreachable
// cloud (fix the network first).
func unreachable(ctx context.Context, err error) *rejectionError {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return &rejectionError{
			Code:    codeUnavailable,
			Message: "The cloud took too long to answer. Nothing was added — try again.",
			cause:   err,
		}
	}
	return &rejectionError{
		Code:    codeUnavailable,
		Message: "Could not reach the cloud to check that streamer. Nothing was added.",
		cause:   err,
	}
}

// blockedRejection refuses an add while this recorder is blocked. It is not a
// verdict on the streamer.
func blockedRejection() *rejectionError {
	return &rejectionError{Code: codeUnavailable, Message: BlockedMessage + ". Nothing was added.", cause: errBlocked}
}

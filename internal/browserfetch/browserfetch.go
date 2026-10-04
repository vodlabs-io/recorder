// Package browserfetch defines the LOCAL-only NATS contract by which the daemon
// asks a running desktop app to execute an HTTP request inside a platform's
// login webview — so the platform's own in-page JS (e.g. TikTok's webmssdk)
// signs it. This is how login-restricted streams whose API requires a signed
// browser context get recorded without the daemon ever reverse-engineering the
// signature.
//
// PRIVACY: like every platform.*/platformlogin.* subject, RequestSubject is
// LOCAL-only: it travels between the daemon and the co-located desktop app over
// the daemon's own NATS server and is NEVER bridged to the cloud. The request
// runs with the webview's own cookies; no cookie value passes through here.
package browserfetch

// RequestSubject is the LOCAL NATS subject (request-reply) on which the daemon
// asks the desktop to run one request in a platform browser context. Headless
// daemons have no subscriber, so the request times out and the caller treats the
// stream as still restricted.
const RequestSubject = "platform.browserfetch.request"

// ReadySubject is the LOCAL NATS subject the desktop announces itself on, once
// per connection, after subscribing to RequestSubject. It is how the daemon
// learns a browser context has appeared, so checks refused for want of one can
// be asked again.
const ReadySubject = "platform.browserfetch.ready"

// Request is the payload the daemon sends. The webview supplies its own cookies,
// User-Agent, and Referer (same-origin), so only URL/method/body are needed.
type Request struct {
	Platform  string `json:"platform"`
	URL       string `json:"url"`
	Method    string `json:"method,omitempty"`
	Body      string `json:"body,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	// ExtractorRegex, when set, is applied to the response body IN-PAGE (inside
	// the webview) before the result is relayed back, keeping the local result
	// channel small: a large HTML page (e.g. a live page's ~400KB SSR) is trimmed
	// to just the matched block instead of being chunked whole. Capture group 1
	// wins when present, else the full match. The pattern is compiled by the
	// page's JS engine, so it must be a valid JS RegExp.
	ExtractorRegex string `json:"extractor_regex,omitempty"`
}

// Response is the desktop's reply: the platform's HTTP response, or an error.
type Response struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SniffSubject is the LOCAL NATS subject (request-reply) on which the daemon
// asks the desktop to load a page in a throwaway browser and report the stream
// requests its player makes. The page is one the user asked to record; the
// daemon refuses any other (see vlstate.PermitSniff). Headless daemons have no
// subscriber, so the request times out and the caller falls back to reading
// the page without a browser.
const SniffSubject = "platform.browsersniff.request"

// SniffRequest asks for one page to be sniffed.
type SniffRequest struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// SniffCandidate is one stream the page's player requested.
type SniffCandidate struct {
	URL string `json:"url"`
	// Format is "hls" or "flv".
	Format string `json:"format"`
	// Frame is the document that made the request, which may be an embedded
	// player from another site. It is the stream's Referer.
	Frame string `json:"frame,omitempty"`
}

// SniffResponse is the desktop's report: the page's title and the streams it
// saw, best first, or an error.
type SniffResponse struct {
	Title      string           `json:"title,omitempty"`
	Candidates []SniffCandidate `json:"candidates"`
	Error      string           `json:"error,omitempty"`
}

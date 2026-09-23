package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/tidwall/gjson"
	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/useragent"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// scrapeDrainTimeout bounds how long a resubscribe waits for in-flight scrapes
// to reply. An ordinary scrape is capped at 10s by its own request context, so
// this clears it with margin without letting a stuck one hold up the reconnect.
const scrapeDrainTimeout = 12 * time.Second

// scrapeHandler handles incoming scrape requests from the cloud
type scrapeHandler struct {
	logger     *vllog.FileLogger
	httpClient *http.Client
	cookies    *platformauth.Store // Local platform logins (PRIVACY: cookies attached here, never sent to cloud)
	// localNC is the daemon's own NATS connection, used to ask a co-located
	// desktop app to run a request in a platform browser context (login webview).
	// Nil-safe: without it (or without a desktop subscriber) browser-context
	// requests fail cleanly and the stream stays restricted.
	localNC *nats.Conn
	// cloudNC is the connection these requests arrive on. Only its negotiated
	// max payload is read, so a reply too big to send fails as an error the
	// backend can see instead of never arriving. Nil-safe.
	cloudNC *nats.Conn
	// checkTarget bounds where a scrape may be sent. Nil means the real guard,
	// checkScrapeTarget; only tests replace it, because their server is on
	// loopback, which is the first thing the guard refuses.
	checkTarget     func(method string, u *url.URL) error
	maxConcurrent   int
	maxPerMinute    int
	currentRequests int
	requestCount    int
	refused         int // scrapes refused in the current minute, for one line about them
	lastMinute      time.Time
	mu              sync.Mutex
	// inflight counts scrapes being served on the subscription's dispatcher;
	// draining stops new ones being taken. A resubscribe closes the cloud
	// connection, and a reply has nowhere to go once it does — so the handler
	// is drained before that happens. idleCh is closed by the last request out.
	inflight int
	draining bool
	idleCh   chan struct{}
}

// begin reserves a slot for a scrape about to be served, reporting false once
// the handler is draining and a reply could no longer be delivered.
func (h *scrapeHandler) begin() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return false
	}
	h.inflight++
	return true
}

// end releases a slot taken by begin, waking drain once the last one is out.
func (h *scrapeHandler) end() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inflight--
	if h.inflight == 0 && h.idleCh != nil {
		close(h.idleCh)
		h.idleCh = nil
	}
}

// drain stops taking scrapes and waits for those in flight to reply, so the
// resubscribe that follows does not close the connection out from under them.
// Past the timeout the reply is abandoned rather than holding up the reconnect.
// A browser-context fetch runs detached and is not waited on.
func (h *scrapeHandler) drain(timeout time.Duration) {
	h.mu.Lock()
	h.draining = true
	if h.inflight == 0 {
		h.mu.Unlock()
		return
	}
	done := make(chan struct{})
	h.idleCh = done
	h.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// newScrapeHandler creates a new scrape handler with a logger and HTTP client
func newScrapeHandler(
	logger *vllog.FileLogger,
	httpClient *http.Client,
	cookies *platformauth.Store,
	localNC *nats.Conn,
	cloudNC *nats.Conn,
) *scrapeHandler {
	return &scrapeHandler{
		logger:        logger,
		httpClient:    httpClient,
		cookies:       cookies,
		localNC:       localNC,
		cloudNC:       cloudNC,
		maxConcurrent: 1,
		maxPerMinute:  scrapeBudgetPerMinute,
		lastMinute:    time.Now(),
	}
}

// scrapeRequest defines the request needed to scrape data from a platform
type scrapeRequest struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	// ExtractorRegex allows the client to extract a subset of the response body.
	ExtractorRegex string `json:"extractor_regex,omitempty"`
	// ExtractorJSON is a JSONPath (gjson syntax) to extract a subset of JSON data.
	ExtractorJSON string `json:"extractor_json,omitempty"`
	// AuthPlatform, when set, asks this client to attach its LOCALLY stored login
	// cookies for the named platform to the outbound request.
	//
	// PRIVACY: the cloud sends only this platform NAME. The cookie values are
	// read from the local encrypted store and attached here, and are only ever
	// sent to that platform's own domains. They never travel back to the cloud.
	AuthPlatform string `json:"auth_platform,omitempty"`
	// BrowserContext asks us to execute this request inside the platform's login
	// webview (via a co-located desktop app) so the platform's own JS signs it,
	// instead of using the local HTTP client. See handleViaBrowser.
	BrowserContext bool `json:"browser_context,omitempty"`
}

// scrapeResponse contains the response from a scrape request
type scrapeResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// checkRateLimit checks if the request rate limit has been exceeded.
//
// takeSlot asks for a concurrency slot as well as budget. A browser-context
// fetch does not take one: it runs in a webview the desktop app owns, which
// bounds how many run at once, and it can take tens of seconds — long enough
// that holding the handler's single slot would refuse every ordinary check
// meanwhile. It is still charged against the per-minute budget, because that is
// what bounds how much work this machine does for the service.
//
// The verdict is reached under the lock and acted on after it: a refusal
// replies over NATS, and holding the handler's lock across that would put every
// concurrent scrape behind one reply.
func (h *scrapeHandler) checkRateLimit(msg *nats.Msg, takeSlot bool) error {
	h.mu.Lock()
	now := time.Now()
	var closed int // refusals in the minute this request just ended
	if now.Sub(h.lastMinute) >= time.Minute {
		closed, h.refused = h.refused, 0
		h.requestCount = 0
		h.lastMinute = now
	}
	var refused string
	switch {
	case h.requestCount >= h.maxPerMinute:
		refused = "rate limit exceeded"
	case takeSlot && h.currentRequests >= h.maxConcurrent:
		refused = "too many concurrent requests"
	default:
		if takeSlot {
			h.currentRequests++
		}
		h.requestCount++
	}
	if refused != "" {
		h.refused++
	}
	first := h.refused == 1
	h.mu.Unlock()

	// A refusal is this daemon pacing the cloud, not a fault, and there is one
	// of them for every request over the budget — so they are reported once as
	// they begin and once as a count, rather than a line each.
	if closed > 0 {
		h.logger.Warn("Scrapes refused over budget", "count", closed, "budget", h.maxPerMinute)
	}
	if refused != "" {
		if first {
			h.logger.Warn("Refusing scrapes", "reason", refused, "budget", h.maxPerMinute)
		}
		h.respondWithError(msg, refused)
		return errors.New(refused)
	}
	return nil
}

// handleScrapeRequest handles a single scrape request
func (h *scrapeHandler) handleScrapeRequest(msg *nats.Msg) {
	log := h.logger.With("func", "handleScrapeRequest")

	// Recover from panics (covers both the browser and HTTP paths).
	defer func() {
		if r := recover(); r != nil {
			log.Error("Scrape handler panic", "error", r)
			h.respondWithError(msg, "internal error")
		}
	}()

	var scrapeReq scrapeRequest
	if err := json.Unmarshal(msg.Data, &scrapeReq); err != nil {
		log.Error("Failed to parse scrape request", "error", err)
		h.respondWithError(msg, "invalid request format")
		return
	}

	log.Info("Scrape request", "method", scrapeReq.Method, "url", scrapeReq.URL)

	parsedURL, err := url.Parse(scrapeReq.URL)
	if err != nil {
		log.Error("Invalid URL", "url", scrapeReq.URL, "error", err)
		h.respondWithError(msg, "invalid URL")
		return
	}
	// What the service may send this machine to fetch, and how. The address it
	// resolves to is checked again at dial time (see scrape_guard.go).
	check := h.checkTarget
	if check == nil {
		check = checkScrapeTarget
	}
	if err := check(scrapeReq.Method, parsedURL); err != nil {
		log.Warn("Refusing scrape request",
			"url", scrapeReq.URL, "method", scrapeReq.Method, "reason", err)
		h.respondWithError(msg, err.Error())
		return
	}

	// Browser-context requests are executed by a co-located desktop app inside
	// the platform's login webview (so the platform's own JS signs them), not by
	// our HTTP client. Run each in its own goroutine so a slow signed fetch never
	// stalls this subscription's serial delivery (ordinary polls, other authed
	// checks); the desktop bounds how many webviews run at once. Headless daemons
	// have no subscriber, so it fails cleanly and the stream stays restricted.
	if scrapeReq.BrowserContext {
		// Charged against the budget before it is handed off, so a refusal is
		// still this subscription's synchronous reply.
		if err := h.checkRateLimit(msg, false); err != nil {
			return
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("Browser scrape panic", "error", r)
					h.respondWithError(msg, "internal error")
				}
			}()
			h.handleViaBrowser(msg, scrapeReq)
		}()
		return
	}

	// Ordinary scrape: enforce the rate limit and hold a concurrency slot.
	// A refusal has already replied and reported itself.
	if err := h.checkRateLimit(msg, true); err != nil {
		return
	}
	defer func() {
		h.mu.Lock()
		h.currentRequests--
		h.mu.Unlock()
	}()

	var bodyReader io.Reader
	if scrapeReq.Body != "" {
		bodyReader = strings.NewReader(scrapeReq.Body)
	}

	timeout := 10 * time.Second
	if scrapeReq.TimeoutMS > 0 && scrapeReq.TimeoutMS <= 30000 {
		timeout = time.Duration(scrapeReq.TimeoutMS) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, scrapeReq.Method, scrapeReq.URL, bodyReader)
	if err != nil {
		log.Error("Failed to create request", "error", err)
		h.respondWithError(msg, fmt.Sprintf("failed to create request: %v", err))
		return
	}

	for key, value := range scrapeReq.Headers {
		req.Header.Set(key, value)
	}

	// Some platforms (TikTok) withhold data from non-browser UAs.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", useragent.Fallback)
	}

	// PRIVACY: attach the stored login's cookies ONLY to the platform's own
	// cookie domains, never log them (count only), and never send them to the
	// cloud, which only ever names the platform.
	if scrapeReq.AuthPlatform != "" && h.cookies != nil {
		if login := vlconfig.LoginFlowFor(scrapeReq.AuthPlatform); login != nil && login.AllowsHost(parsedURL.Host) {
			if sess, ok := h.cookies.Get(scrapeReq.AuthPlatform); ok {
				req.Header.Set("Cookie", sess.Header())
				log.Info("Attached platform login for authenticated check",
					"platform", scrapeReq.AuthPlatform,
					"host", parsedURL.Host,
					"cookie_count", len(sess.Cookies))
				// Some platforms need more than the jar: YouTube's InnerTube
				// wants a signature derived from one of the cookies. Only this
				// machine can compute it — see platformauth.Session.AuthHeaders.
				if a := login.Auth; a != nil {
					extra := sess.AuthHeaders(a.Scheme, a.Cookies, a.Origin)
					for k, v := range extra {
						req.Header.Set(k, v)
					}
					switch {
					case len(extra) > 0:
						log.Info("Signed authenticated check", "platform", scrapeReq.AuthPlatform,
							"scheme", a.Scheme)
					case !sess.HasAuthSecret(a.Cookies):
						// The capture dropped the cookie the scheme signs with,
						// so this login can never authenticate. Say so plainly:
						// the platform will answer as if signed out, and that
						// looks like an expired login instead of a bad capture.
						log.Warn("Stored login cannot be signed; the platform will treat it as signed out",
							"platform", scrapeReq.AuthPlatform, "scheme", a.Scheme)
					}
				}
			} else {
				log.Info("Authenticated check requested but no stored login",
					"platform", scrapeReq.AuthPlatform)
			}
		} else {
			// Normal for a check that also fetches from a media host outside the
			// cookie domains (Twitch: usher.ttvnw.net), so not a warning.
			log.Info("Not attaching login: host is outside the platform's cookie domains",
				"platform", scrapeReq.AuthPlatform, "host", parsedURL.Host)
		}
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Error("Request failed", "error", err)
		h.respondWithError(msg, fmt.Sprintf("request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	// Bounds memory, not the reply (which the cloud's payload limit caps). Must
	// comfortably exceed the largest page a plugin scrapes, e.g. Douyin's
	// ~1.4MB live page.
	const maxBodySize = 8 * 1024 * 1024
	limitedReader := io.LimitReader(resp.Body, maxBodySize)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		log.Error("Failed to read response", "error", err)
		h.respondWithError(msg, fmt.Sprintf("failed to read response: %v", err))
		return
	}

	extracted := false
	if scrapeReq.ExtractorRegex != "" {
		re, err := regexp.Compile(scrapeReq.ExtractorRegex)
		if err != nil {
			log.Error("Invalid extractor regex", "regex", scrapeReq.ExtractorRegex, "error", err)
			h.respondWithError(msg, "invalid extractor regex")
			return
		}
		matches := re.FindSubmatch(body)
		if len(matches) >= 2 {
			body = matches[1]
			extracted = true
		} else if len(matches) == 1 {
			body = matches[0]
			extracted = true
		} else {
			log.Error("Extractor regex did not match", "regex", scrapeReq.ExtractorRegex)
			h.respondWithError(msg, "extractor regex did not match")
			return
		}
	}

	// Apply extractor JSONPath if provided (uses gjson)
	if scrapeReq.ExtractorJSON != "" {
		result := gjson.GetBytes(body, scrapeReq.ExtractorJSON)
		if !result.Exists() {
			log.Error("Extractor json path did not match", "json_path", scrapeReq.ExtractorJSON)
			h.respondWithError(msg, "extractor json path did not match")
			return
		}
		body = []byte(result.Raw)
		extracted = true
	}

	headers := replyHeaders(resp.Header, extracted)

	scrapeResp := scrapeResponse{
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    string(body),
	}

	if h.respond(msg, scrapeResp) {
		log.Info("Scrape response",
			"status", resp.StatusCode,
			"status_text", http.StatusText(resp.StatusCode),
			"body_length", len(body))
	}
}

// scrapeReplyHeaders are the response headers a scrape reply may carry.
var scrapeReplyHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Location",
}

// replyHeaders picks the headers that travel back with a scrape.
//
// It is an allowlist. Everything else is the platform's answer to THIS
// machine — Set-Cookie above all, which can carry a session or an anti-bot
// token minted for this address — and a reply is the wrong place for it. The
// list is short because a check reads exactly two: Content-Type, and the
// extracted marker.
func replyHeaders(h http.Header, extracted bool) map[string]string {
	headers := make(map[string]string)
	for _, key := range scrapeReplyHeaders {
		if v := h.Get(key); v != "" {
			headers[key] = v
		}
	}
	if extracted {
		headers["X-Scrape-Extracted"] = "true"
	}
	return headers
}

// respond replies to a scrape request, and reports whether the reply was sent.
func (h *scrapeHandler) respond(msg *nats.Msg, resp scrapeResponse) bool {
	log := h.logger.With("func", "respond")

	respData, err := json.Marshal(resp)
	if err != nil {
		log.Error("Failed to marshal response", "error", err)
		h.respondWithError(msg, "failed to marshal response")
		return false
	}

	if max := h.maxPayload(); max > 0 && int64(len(respData)) > max {
		log.Error("Scrape response exceeds NATS max payload",
			"response_bytes", len(respData), "max_payload", max,
			"body_length", len(resp.Body))
		h.respondWithError(msg, fmt.Sprintf(
			"response too large to relay: %d bytes (max %d)", len(respData), max))
		return false
	}

	if err := msg.Respond(respData); err != nil {
		log.Error("Failed to send scrape response", "error", err)
		return false
	}
	return true
}

// maxPayload returns the payload ceiling the cloud NATS server negotiated, or 0
// when it is unknown (no connection).
func (h *scrapeHandler) maxPayload() int64 {
	if h.cloudNC == nil {
		return 0
	}
	return h.cloudNC.MaxPayload()
}

// handleViaBrowser forwards a browser-context request to a co-located desktop
// app over LOCAL NATS, which runs it inside the platform's login webview (so the
// platform's own JS signs it), and relays the platform response back to the
// backend. If no desktop is listening (headless daemon, or app not running) the
// request times out and we return an error, leaving the stream restricted.
//
// PRIVACY: only the platform NAME and the cloud-supplied URL travel locally to
// the desktop; the request runs with the webview's own cookies.
//
// PRIVACY: a page fetched inside a login webview is rendered FOR THE SIGNED-IN
// VIEWER, so it can carry the user's own account beside whatever was asked
// about. The reply therefore has to name what it needs (ExtractorJSON), which
// is applied here, on this machine: a request that names nothing is refused
// rather than answered with the page.
func (h *scrapeHandler) handleViaBrowser(msg *nats.Msg, scrapeReq scrapeRequest) {
	log := h.logger.With("func", "handleViaBrowser")

	if scrapeReq.ExtractorJSON == "" {
		log.Warn("Refusing a browser-context request that names no fields",
			"platform", scrapeReq.AuthPlatform, "url", scrapeReq.URL)
		h.respondWithError(msg, "a browser-context request must name the fields it needs")
		return
	}

	if h.localNC == nil {
		log.Warn("Browser-context request but no local NATS connection", "platform", scrapeReq.AuthPlatform)
		h.respondWithError(msg, "browser context unavailable")
		return
	}

	timeout := 30 * time.Second
	if scrapeReq.TimeoutMS > 0 && time.Duration(scrapeReq.TimeoutMS)*time.Millisecond > timeout {
		timeout = time.Duration(scrapeReq.TimeoutMS) * time.Millisecond
	}

	reqData, err := json.Marshal(browserfetch.Request{
		Platform:       scrapeReq.AuthPlatform,
		URL:            scrapeReq.URL,
		Method:         scrapeReq.Method,
		Body:           scrapeReq.Body,
		TimeoutMS:      int(timeout / time.Millisecond),
		ExtractorRegex: scrapeReq.ExtractorRegex,
	})
	if err != nil {
		h.respondWithError(msg, "failed to marshal browser request")
		return
	}

	log.Info("Forwarding request to browser context",
		"platform", scrapeReq.AuthPlatform, "url", scrapeReq.URL)

	reply, err := h.localNC.Request(browserfetch.RequestSubject, reqData, timeout)
	if err != nil {
		// No desktop subscriber (headless), app closed, or the webview timed out.
		log.Info("Browser context unavailable", "platform", scrapeReq.AuthPlatform, "error", err)
		h.respondWithError(msg, "browser context unavailable: "+err.Error())
		return
	}

	var bfResp browserfetch.Response
	if err := json.Unmarshal(reply.Data, &bfResp); err != nil {
		h.respondWithError(msg, "invalid browser response")
		return
	}
	if bfResp.Error != "" {
		log.Info("Browser context returned error", "platform", scrapeReq.AuthPlatform, "error", bfResp.Error)
		h.respondWithError(msg, "browser fetch failed: "+bfResp.Error)
		return
	}

	// The webview applied the regex in-page, if there was one; the reply is
	// narrowed to the named fields here before it leaves the machine.
	extracted, err := narrowBrowserBody(bfResp.Body, scrapeReq.ExtractorJSON)
	if err != nil {
		log.Info("Browser context response did not hold the named fields",
			"platform", scrapeReq.AuthPlatform, "json_path", scrapeReq.ExtractorJSON,
			"status", bfResp.Status, "body_length", len(bfResp.Body))
		h.respondWithError(msg, err.Error())
		return
	}

	log.Info("Browser context response",
		"platform", scrapeReq.AuthPlatform,
		"url", scrapeReq.URL,
		"status", bfResp.Status,
		"body_length", len(bfResp.Body),
		"extracted_length", len(extracted))

	h.respond(msg, scrapeResponse{
		Status:  bfResp.Status,
		Headers: map[string]string{"X-Scrape-Extracted": "true"},
		Body:    extracted,
	})
}

// narrowBrowserBody keeps the named fields of a browser-context response and
// discards the rest. A path that matches nothing is an error rather than a
// whole page: see handleViaBrowser for why the page itself may never travel.
func narrowBrowserBody(body, jsonPath string) (string, error) {
	if jsonPath == "" {
		return "", errors.New("a browser-context request must name the fields it needs")
	}
	extracted := gjson.Get(body, jsonPath)
	if !extracted.Exists() {
		return "", errors.New("extractor json path did not match")
	}
	return extracted.Raw, nil
}

// respondWithError sends an error response
func (h *scrapeHandler) respondWithError(msg *nats.Msg, errMsg string) {
	log := h.logger.With("func", "respondWithError")
	resp := scrapeResponse{Error: errMsg}
	respData, _ := json.Marshal(resp)
	if err := msg.Respond(respData); err != nil {
		log.Error("Failed to send error response", "error", err)
	}
}

// startScrapeHandler starts listening for scrape requests
func (c *Client) startScrapeHandler(ctx context.Context) error {
	log := c.logger.With("func", "startScrapeHandler")

	scrapeLogger, err := vllog.NewFileLogger(c.scraperConfig.Logger, "scraper")
	if err != nil {
		return fmt.Errorf("failed to initialize scrape logger: %v", err)
	}
	// Scrapes get their own guarded client rather than the one the daemon calls
	// the service with: that one may legitimately reach a private address when a
	// developer runs the service locally, and a scrape never may.
	handler := newScrapeHandler(scrapeLogger, newScrapeClient(), c.cookies, c.localNC, c.nc)

	sub, err := c.nc.Subscribe("scrape.requests", func(msg *nats.Msg) {
		c.waitSubscribe()
		// Refuse once draining: an error now beats the cloud waiting out its
		// own deadline for a reply that can't be delivered.
		if !handler.begin() {
			handler.respondWithError(msg, "daemon is reconnecting")
			return
		}
		defer handler.end()
		handler.handleScrapeRequest(msg)
	})
	if err != nil {
		return fmt.Errorf("failed to subscribe to scrape requests: %w", err)
	}
	scrapeLogger.Info("Started scrape handler")

	scrapeCtx, cancel := context.WithCancel(ctx)
	c.stopScrapeHandler = cancel

	// Tracked by natsHandlerWg, which newSubscription waits on before closing
	// the old connection, so in-flight replies still go out.
	c.natsHandlerWg.Go(func() {
		<-scrapeCtx.Done()
		sub.Unsubscribe()
		handler.drain(scrapeDrainTimeout)
		log.Info("Stopped scrape handler")
		scrapeLogger.Info("Stopped scrape handler")
		scrapeLogger.Close()
	})

	log.Info("Started scrape handler")
	return nil
}

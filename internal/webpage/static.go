package webpage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/vodlabs-io/recorder/internal/useragent"
)

const (
	// staticBudget bounds a whole static check: the page and every probe.
	staticBudget = 15 * time.Second
	// maxPageBytes bounds the HTML read from a page.
	maxPageBytes = 2 << 20
	// maxPlaylistBytes bounds a playlist read by a probe.
	maxPlaylistBytes = 1 << 20
	// maxProbes is how many candidates a check probes before giving up.
	maxProbes = 3
)

var (
	// absStreamRx finds absolute or protocol-relative playlist addresses in
	// page text: inline player configs, JSON state, attributes. The path is
	// matched greedily, so "/live.flv/playlist.m3u8" is one address, not two.
	absStreamRx = regexp.MustCompile(`(?i)(?:https?:)?//[^\s"'<>\\?` + "`" + `]+\.(?:m3u8|flv)(?:\?[^\s"'<>\\` + "`" + `]*)?`)
	// mediaSrcRx finds <video> and <source> src attributes, which may be relative.
	mediaSrcRx = regexp.MustCompile(`(?i)<(?:video|source)\b[^>]*?\ssrc\s*=\s*["']([^"']+)["']`)
	metaRx     = regexp.MustCompile(`(?i)<meta\b[^>]*>`)
	attrRx     = regexp.MustCompile(`(?i)([a-z:_-]+)\s*=\s*["']([^"']*)["']`)
	titleRx    = regexp.MustCompile(`(?is)<title[^>]*>([^<]*)</title>`)
)

// finding is what a check learned about a page.
type finding struct {
	live     bool
	stream   string // the stream to record, when live
	format   string // formatHLS or formatFLV
	headers  map[string]string
	title    string
	siteName string
}

// readStatic finds the stream by reading the page's HTML, with no JavaScript,
// and probes each candidate before believing it.
func readStatic(ctx context.Context, client *http.Client, page *url.URL, log *slog.Logger) (finding, error) {
	ctx, cancel := context.WithTimeout(ctx, staticBudget)
	defer cancel()

	var (
		f          finding
		candidates []string
		referer    *url.URL
	)
	if formatOf(page.Path) != "" {
		// The stream's own address. Probed like any candidate, so a VOD
		// playlist or a dead link reads offline.
		candidates = []string{page.String()}
	} else {
		resp, body, err := get(ctx, client, page.String(), "", maxPageBytes)
		if err != nil {
			return f, fmt.Errorf("fetch page: %w", err)
		}
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			return f, nil
		case resp.StatusCode >= 400:
			return f, fmt.Errorf("fetch page: %s", resp.Status)
		}
		final := resp.Request.URL
		if sniffBody(resp, body) != "" {
			// A stream address with no telltale extension.
			return probed(ctx, client, f, []string{final.String()}, nil, log)
		}
		m := pageMeta(body)
		f.title = m.title
		f.siteName = m.siteName
		candidates = extractCandidates(body, final)
		referer = final
	}
	return probed(ctx, client, f, candidates, referer, log)
}

// probed probes candidates in order and fills f from the first that is live.
func probed(
	ctx context.Context,
	client *http.Client,
	f finding,
	candidates []string,
	referer *url.URL,
	log *slog.Logger,
) (finding, error) {
	headers := refererHeaders(referer)
	var (
		lastErr  error
		answered bool
	)
	for i, c := range candidates {
		if i == maxProbes {
			break
		}
		stream, format, err := probe(ctx, client, c, headers)
		if err != nil {
			log.Debug("Candidate probe failed", "url", c, "error", err)
			lastErr = err
			continue
		}
		answered = true
		if stream != "" {
			f.live, f.stream, f.format, f.headers = true, stream, format, headers
			return f, nil
		}
	}
	// Every probe failing in transit says nothing about the stream.
	if lastErr != nil && !answered {
		return f, fmt.Errorf("probe stream: %w", lastErr)
	}
	return f, nil
}

// probe fetches a candidate and returns the address to record, or "" when it
// is not a live stream: gone, refused, a VOD, or not a stream at all.
func probe(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) (string, string, error) {
	if _, ok := streamURL(rawURL); !ok {
		return "", "", nil
	}
	resp, body, err := get(ctx, client, rawURL, headers["Referer"], maxPlaylistBytes)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 300 {
		return "", "", nil
	}
	final := resp.Request.URL.String()
	switch sniffBody(resp, body) {
	case formatFLV:
		return final, formatFLV, nil
	case formatHLS:
		live, err := hlsLive(ctx, client, final, body, headers)
		if err != nil || !live {
			return "", "", err
		}
		return final, formatHLS, nil
	}
	return "", "", nil
}

// hlsLive reads a playlist and says whether it is live. A master playlist is
// followed to its first variant, since only a media playlist can say it has
// ended. The master itself is what gets recorded: the HLS writer picks the
// variant.
func hlsLive(ctx context.Context, client *http.Client, playlistURL string, body []byte, headers map[string]string) (bool, error) {
	pl, kind, err := m3u8.Decode(*bytes.NewBuffer(body), false)
	if err != nil {
		return false, nil
	}
	if kind == m3u8.MEDIA {
		return !mediaEnded(pl.(*m3u8.MediaPlaylist)), nil
	}
	master := pl.(*m3u8.MasterPlaylist)
	var variant string
	for _, v := range master.Variants {
		if v != nil && !v.Iframe && v.URI != "" {
			variant = v.URI
			break
		}
	}
	base, err := url.Parse(playlistURL)
	if err != nil || variant == "" {
		return false, nil
	}
	ref, err := url.Parse(variant)
	if err != nil {
		return false, nil
	}
	resp, vbody, err := get(ctx, client, base.ResolveReference(ref).String(), headers["Referer"], maxPlaylistBytes)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 300 {
		return false, nil
	}
	vpl, vkind, err := m3u8.Decode(*bytes.NewBuffer(vbody), false)
	if err != nil || vkind != m3u8.MEDIA {
		return false, nil
	}
	return !mediaEnded(vpl.(*m3u8.MediaPlaylist)), nil
}

// mediaEnded reports a media playlist that will not grow: a finished
// broadcast or a VOD.
func mediaEnded(pl *m3u8.MediaPlaylist) bool {
	return pl.Closed || pl.MediaType == m3u8.VOD
}

// sniffBody names the stream format a response carries, or "" when it is not
// a stream.
func sniffBody(resp *http.Response, body []byte) string {
	b := bytes.TrimPrefix(bytes.TrimSpace(body), []byte("\ufeff"))
	switch {
	case bytes.HasPrefix(b, []byte("#EXTM3U")):
		return formatHLS
	case bytes.HasPrefix(body, []byte("FLV")):
		return formatFLV
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "x-flv") {
		return formatFLV
	}
	return ""
}

// get makes one GET with a browser's headers and reads at most limit bytes of
// the body. An FLV stream never ends, so the read stops at the limit rather
// than at EOF.
func get(ctx context.Context, client *http.Client, rawURL, referer string, limit int64) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", useragent.Fallback)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/vnd.apple.mpegurl,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if formatOf(rawURL) == formatFLV || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "flv") {
		limit = 16
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && len(body) == 0 {
		return nil, nil, err
	}
	return resp, body, nil
}

// extractCandidates lists the stream addresses a page names, in the order
// they are worth probing: what a <video> or <source> plays, what og:video
// names, then any playlist address anywhere in the text.
func extractCandidates(body []byte, base *url.URL) []string {
	text := strings.NewReplacer(
		`\/`, "/", `/`, "/", `&`, "&", "&amp;", "&", "&#x27;", "'",
	).Replace(string(body))
	var out []string
	seen := map[string]bool{}
	add := func(raw string) {
		ref, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
		if err != nil {
			return
		}
		abs := base.ResolveReference(ref)
		s := abs.String()
		if seen[s] || formatOf(s) == "" {
			return
		}
		if _, ok := streamURL(s); !ok {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range mediaSrcRx.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, tag := range metaRx.FindAllString(text, -1) {
		attrs := tagAttrs(tag)
		switch attrs["property"] {
		case "og:video", "og:video:url", "og:video:secure_url":
			add(attrs["content"])
		}
	}
	for _, m := range absStreamRx.FindAllString(text, -1) {
		add(m)
	}
	return out
}

type meta struct {
	title, siteName string
}

// pageMeta reads the title and site name a page declares.
func pageMeta(body []byte) meta {
	var m meta
	for _, tag := range metaRx.FindAll(body, -1) {
		attrs := tagAttrs(string(tag))
		key := attrs["property"]
		if key == "" {
			key = attrs["name"]
		}
		content := strings.TrimSpace(html.UnescapeString(attrs["content"]))
		switch key {
		case "og:title":
			m.title = content
		case "og:site_name":
			m.siteName = content
		}
	}
	if m.title == "" {
		if t := titleRx.FindSubmatch(body); t != nil {
			m.title = strings.TrimSpace(html.UnescapeString(string(t[1])))
		}
	}
	return m
}

func tagAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	for _, a := range attrRx.FindAllStringSubmatch(tag, -1) {
		attrs[strings.ToLower(a[1])] = a[2]
	}
	return attrs
}

package vlupdate

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// LatestVersion returns the latest release tag for a GitHub repo (without the
// leading "v"). It reads the Location header from <repo>/releases/latest,
// which GitHub serves as a 302 redirect to /releases/tag/v<X.Y.Z> — no API
// token required.
func LatestVersion(ctx context.Context, repo string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("binary repo is empty")
	}
	url := strings.TrimRight(repo, "/") + "/releases/latest"

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no Location header from %s (HTTP %d)", url, resp.StatusCode)
	}
	tag := path.Base(loc)
	if tag == "" || tag == "/" || tag == "." {
		return "", fmt.Errorf("malformed redirect target %q", loc)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

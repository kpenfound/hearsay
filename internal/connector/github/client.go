package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxResponse bounds what one REST response may be. A page of a hundred issues
// is a few hundred kilobytes; this is a guard against a broken proxy, not a
// budget.
const maxResponse = 32 << 20

// client is the part of GitHub's REST API the connector calls: authenticated
// GETs.
type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// statusError is a REST call GitHub answered with something other than success.
type statusError struct {
	path   string
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: GitHub answered %d %s", e.path, e.status, http.StatusText(e.status))
}

// get fetches rel — a path and query relative to the API's base URL — into v,
// and reports whether GitHub says there is a next page.
//
// The connector builds every page's URL itself rather than following GitHub's
// next link, so a cursor holds a position rather than a URL and the token only
// ever goes to the configured API.
func (c *client) get(ctx context.Context, rel string, v any) (bool, error) {
	path, _, _ := strings.Cut(rel, "?")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+rel, nil)
	if err != nil {
		return false, fmt.Errorf("building GET %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "hearsay")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body is GitHub's explanation, and is drained rather than quoted: it
		// is served to a log line, and the status says enough to act on.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return false, &statusError{path: path, status: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(v); err != nil {
		return false, fmt.Errorf("decoding GET %s: %w", path, err)
	}
	return hasNext(resp.Header.Get("Link")), nil
}

// hasNext reports whether a Link header names a rel="next" page.
func hasNext(header string) bool {
	for part := range strings.SplitSeq(header, ",") {
		_, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			if strings.TrimSpace(param) == `rel="next"` {
				return true
			}
		}
	}
	return false
}

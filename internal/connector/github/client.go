package github

import (
	"context"
	"encoding/json"
	"errors"
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
// GETs, paged by the Link header.
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
// and returns the next page's link, relative the same way, or "" on the last
// page.
func (c *client) get(ctx context.Context, rel string, v any) (string, error) {
	path, _, _ := strings.Cut(rel, "?")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+rel, nil)
	if err != nil {
		return "", fmt.Errorf("building GET %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "hearsay")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body is GitHub's explanation, and is drained rather than quoted: it
		// is served to a log line, and the status says enough to act on.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return "", &statusError{path: path, status: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(v); err != nil {
		return "", fmt.Errorf("decoding GET %s: %w", path, err)
	}
	next, err := c.relative(nextLink(resp.Header.Get("Link")))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	return next, nil
}

// relative turns a next-page link into a path relative to the base URL. A link
// anywhere else is refused rather than followed: the token goes wherever the
// connector calls, and a cursor must not carry a host.
func (c *client) relative(link string) (string, error) {
	if link == "" {
		return "", nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("next-page link is not a URL: %w", err)
	}
	if u.Scheme != c.base.Scheme || u.Host != c.base.Host || !strings.HasPrefix(u.Path, c.base.Path+"/") {
		return "", errors.New("next-page link points outside the configured API")
	}
	rel := strings.TrimPrefix(u.Path, c.base.Path)
	if u.RawQuery != "" {
		rel += "?" + u.RawQuery
	}
	return rel, nil
}

// nextLink is the rel="next" URL of a Link header, or "".
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			if strings.TrimSpace(param) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(target), "<>")
			}
		}
	}
	return ""
}

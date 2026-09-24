package github

import (
	"bytes"
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

// client is the part of GitHub's REST API Hearsay calls: authenticated GETs,
// and the POST and PATCH a [Replier] answers a command with.
type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// statusError is a REST call GitHub answered with something other than success.
type statusError struct {
	method string
	path   string
	status int
}

func (e *statusError) Error() string {
	method := e.method
	if method == "" {
		method = http.MethodGet
	}
	return fmt.Sprintf("%s %s: GitHub answered %d %s", method, e.path, e.status, http.StatusText(e.status))
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

// repository reads a repository's metadata. Every walk, visibility check and
// read of a repository starts here, so a repository the token cannot see fails
// the same way whichever of them asked.
func (c *client) repository(ctx context.Context, fullName string) (repository, error) {
	var repo repository
	if _, err := c.get(ctx, repoPath(fullName), &repo); err != nil {
		return repository{}, fmt.Errorf("reading repository %s: %w", fullName, err)
	}
	return repo, nil
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

// send makes a write — rel relative to the API's base URL, in as its JSON
// body — and decodes GitHub's answer into out, when it is a success.
func (c *client) send(ctx context.Context, method, rel string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encoding %s %s: %w", method, rel, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+rel, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, rel, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "hearsay")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, rel, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return &statusError{method: method, path: rel, status: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s: %w", method, rel, err)
	}
	return nil
}

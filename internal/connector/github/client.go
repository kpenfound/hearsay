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

const maxResponse = 32 << 20

type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

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
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return false, &statusError{path: path, status: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(v); err != nil {
		return false, fmt.Errorf("decoding GET %s: %w", path, err)
	}
	return hasNext(resp.Header.Get("Link")), nil
}

func (c *client) repository(ctx context.Context, fullName string) (repository, error) {
	var repo repository
	if _, err := c.get(ctx, repoPath(fullName), &repo); err != nil {
		return repository{}, fmt.Errorf("reading repository %s: %w", fullName, err)
	}
	return repo, nil
}

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

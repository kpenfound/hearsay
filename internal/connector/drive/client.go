package drive

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxBody = 16 << 20

type client struct {
	base, tokenURI, email string
	key                   *rsa.PrivateKey
	http                  *http.Client
	mu                    sync.Mutex
	token                 string
	expires               time.Time
}

func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	now := time.Now()
	encode := base64.RawURLEncoding.EncodeToString
	header := encode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iss": c.email, "scope": "https://www.googleapis.com/auth/drive.readonly", "aud": c.tokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	assertion := header + "." + encode(claims)
	digest := sha256.Sum256([]byte(assertion))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing Drive credential: %w", err)
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion + "." + encode(signature)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting Drive token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("requesting Drive token: status %d", resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&token); err != nil {
		return "", fmt.Errorf("decoding Drive token: %w", err)
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 {
		return "", fmt.Errorf("drive token response lacks access_token or expires_in")
	}
	c.token, c.expires = token.AccessToken, now.Add(time.Duration(token.ExpiresIn)*time.Second)
	return c.token, nil
}

type statusError struct {
	status int
	path   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: Drive answered %d", e.path, e.status)
}

func (c *client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building GET %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{resp.StatusCode, path}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading GET %s: %w", path, err)
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("GET %s exceeds %d bytes", path, maxBody)
	}
	return data, nil
}

func (c *client) getJSON(ctx context.Context, path string, query url.Values, v any) error {
	data, err := c.get(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(v); err != nil {
		return fmt.Errorf("decoding GET %s: %w", path, err)
	}
	return nil
}

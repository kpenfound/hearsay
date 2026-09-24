// Package slack ingests the public channels of one Slack workspace through a
// Socket Mode connection (ADR-0015): the connector dials Slack, so a
// deployment needs no public ingress and no request URL.
//
// A source names the workspace in settings `team` (its `T…` id) and its
// channels in `containers`, by channel id (`C…`). Configure public channels;
// when one becomes private or archived, its prior content is restricted. A `D…` (direct message) or `G…` (private channel or
// group direct message) container, or `*`, fails construction, and so a
// configuration naming one does not start. Private channels created since 2021
// have `C…` ids too, so every connection first reads each configured channel
// with `conversations.info`: a private or archived channel is restricted by a
// durable L0 re-sync. A direct message or Slack Connect channel stops the
// source with failed health. A
// channel the app has not been invited to is reported as degraded health and
// checked again on the next connection. Events from any other channel class
// (`channel_type` other than `channel`, or `is_ext_shared_channel`) and from
// another workspace are acknowledged and dropped.
//
// Two secrets name environment variables: `app_token`, the app-level token
// (`xapp-…`, scope `connections:write`) that opens the socket, and
// `bot_token`, the bot user OAuth token (`xoxb-…`) that reads channels. The
// runtime resolves both before calling Factory. `api_url` replaces
// `https://slack.com/api` for a local fixture.
//
// Setting the app up: create an app from the manifest below at
// https://api.slack.com/apps, generate an app-level token with
// `connections:write` under Basic Information, install the app to the
// workspace and copy its Bot User OAuth Token, then `/invite` the app into
// every channel the source names. Slack delivers channel events only to an app
// that is a member of the channel.
//
//	display_information:
//	  name: Hearsay
//	features:
//	  bot_user:
//	    display_name: Hearsay
//	    always_online: false
//	oauth_config:
//	  scopes:
//	    bot:
//	      - channels:history
//	      - channels:read
//	      - reactions:read
//	settings:
//	  event_subscriptions:
//	    bot_events:
//	      - message.channels
//	      - reaction_added
//	      - reaction_removed
//	      - channel_archive
//	      - channel_unarchive
//	      - channel_deleted
//	  interactivity:
//	    is_enabled: false
//	  org_deploy_enabled: false
//	  socket_mode_enabled: true
//	  token_rotation_enabled: false
//
// Every scope is read-only: the connector never writes to Slack, so a source
// configured `read_only: true` is ingested exactly the same way. Reactions are
// ingested as `reaction` events, and nothing reads them as gestures yet.
//
// What the connector emits is in docs/connector-contract.md (Slack). Messages,
// thread replies, edits, deletions and reactions are ingested; a reply's
// `thread` and `parent` are the message it answers. The runtime retries a
// connection that ends with backoff, and the connector opens a new one itself
// when Slack asks it to refresh. Backfill and ACL re-sync use runtime-managed
// cursors. `/readyz` reports the socket state.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
)

const (
	// Type is the connector type registered by the binary.
	Type = "slack"
	// SecretAppToken names the app-level token that opens Socket Mode.
	SecretAppToken = "app_token"
	// SecretBotToken names the bot token that reads channels.
	SecretBotToken = "bot_token"

	defaultAPI = "https://slack.com/api"
	// keepalive is how often the connector pings Slack. A connection that
	// shows no frame for three of these is treated as dead.
	keepalive = 30 * time.Second
)

// errRefresh ends a connection Slack asked the connector to replace, which
// Stream does at once rather than through the runtime's backoff.
var errRefresh = errors.New("slack asked for a new connection")

// Settings configures a workspace's Socket Mode stream.
type Settings struct {
	// Team is the workspace id. Events from any other workspace are dropped.
	Team string `json:"team"`
	// APIURL replaces Slack's Web API base, for a local fixture.
	APIURL string `json:"api_url"`
}

// Connector holds one Socket Mode connection for a source.
type Connector struct {
	source, team, api  string
	appToken, botToken string
	http               *http.Client
	containers         []string
	mu                 sync.Mutex
	status             connector.HealthStatus
	detail             string
	last               time.Time
	conn               *websocket.Conn
	stop               context.CancelFunc
	active             chan struct{}
	closed             bool
	notMember          []string
	restricted         map[string]bool
	nextRequest        time.Time
}

var _ connector.Streamer = (*Connector)(nil)
var _ connector.Backfiller = (*Connector)(nil)
var _ connector.Resyncer = (*Connector)(nil)

// Factory builds a Slack connector for the runtime registry.
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New validates a resolved source and returns an idle connector. It makes no
// network call: the channel checks that need Slack run on every connection.
func New(src connector.SourceConfig) (*Connector, error) {
	var s Settings
	if err := src.DecodeSettings(&s); err != nil {
		return nil, err
	}
	if !slackID(s.Team, 'T') {
		return nil, errors.New("slack setting team must be a workspace id (T…)")
	}
	api, err := apiURL(s.APIURL)
	if err != nil {
		return nil, err
	}
	for name := range src.Secrets {
		if name != SecretAppToken && name != SecretBotToken {
			return nil, fmt.Errorf("slack does not read secret %q", name)
		}
	}
	if !strings.HasPrefix(src.Secrets[SecretAppToken], "xapp-") {
		return nil, errors.New("slack secret app_token is required and must be an app-level token (xapp-…)")
	}
	if !strings.HasPrefix(src.Secrets[SecretBotToken], "xoxb-") {
		return nil, errors.New("slack secret bot_token is required and must be a bot token (xoxb-…)")
	}
	if len(src.Containers) == 0 {
		return nil, errors.New("slack containers must name at least one public channel")
	}
	for _, id := range src.Containers {
		if err := channelClass(id); err != nil {
			return nil, err
		}
	}
	containers := slices.Clone(src.Containers)
	slices.Sort(containers)
	return &Connector{
		source: src.ID, team: s.Team, api: api,
		appToken: src.Secrets[SecretAppToken], botToken: src.Secrets[SecretBotToken],
		http:       &http.Client{Timeout: 30 * time.Second},
		containers: containers,
		restricted: map[string]bool{},
		status:     connector.HealthDegraded, detail: "connecting",
	}, nil
}

// channelClass refuses a container whose id alone says it is not a public
// channel. Slack's id prefixes: C for a channel, D for a direct message, G for
// a private channel or group direct message created before 2021.
func channelClass(id string) error {
	switch {
	case id == connector.AllowAll:
		return errors.New("slack containers must name public channel ids, not *")
	case slackID(id, 'D'):
		return fmt.Errorf("slack container %q is a direct message: only configured public-channel ids are supported", id)
	case slackID(id, 'G'):
		return fmt.Errorf("slack container %q is a private channel or group direct message: only configured public-channel ids are supported", id)
	case !slackID(id, 'C'):
		return fmt.Errorf("slack container %q must be a public channel id (C…)", id)
	}
	return nil
}

// slackID reports an id of Slack's shape: a type letter, then upper-case
// letters and digits.
func slackID(s string, prefix byte) bool {
	if len(s) < 3 || s[0] != prefix {
		return false
	}
	for _, r := range s[1:] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// apiURL is the Web API base a source's settings name, without a trailing
// slash.
func apiURL(api string) (string, error) {
	if api == "" {
		api = defaultAPI
	}
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("slack api_url must be an HTTP URL without query or fragment")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		return "", errors.New("slack api_url must use https outside localhost")
	}
	return strings.TrimRight(api, "/"), nil
}

// Describe declares the kinds the stream emits. There is no `command`: this
// version answers no slash command, whatever `read_only` says.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindMessage, connector.KindReaction, connector.KindTombstone}}
}

// Health reports the cached socket state without network IO.
func (c *Connector) Health(context.Context) connector.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return connector.Health{Status: c.status, Detail: c.detail, LastEventAt: c.last}
}

func (c *Connector) setHealth(status connector.HealthStatus, detail string) {
	c.mu.Lock()
	c.status = status
	c.detail = detail
	c.mu.Unlock()
}

// Close stops the socket and waits for Stream to return.
func (c *Connector) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	conn, active, stop := c.conn, c.active, c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if active != nil {
		select {
		case <-active:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Stream opens Socket Mode connections and reads events until one ends in a
// way Slack did not ask for. Slack's own refresh (a `disconnect` envelope) is
// answered with a new connection here, without returning.
func (c *Connector) Stream(ctx context.Context, sink connector.Sink) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return connector.ErrClosed
	}
	if c.active != nil {
		c.mu.Unlock()
		return errors.New("slack stream already running")
	}
	active := make(chan struct{})
	c.active = active
	ctx, cancel := context.WithCancel(ctx)
	c.stop = cancel
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.active = nil
		c.stop = nil
		c.mu.Unlock()
		close(active)
	}()
	for {
		err := c.session(ctx, sink)
		if errors.Is(err, errRefresh) && ctx.Err() == nil {
			continue
		}
		c.mu.Lock()
		if ctx.Err() == nil && c.status != connector.HealthFailed {
			c.status = connector.HealthDegraded
			c.detail = "reconnecting"
		}
		c.mu.Unlock()
		return err
	}
}

// envelope is one Socket Mode frame.
type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason"`
}

// session is one connection: check the channels, open, read hello, then
// events until it ends.
func (c *Connector) session(ctx context.Context, sink connector.Sink) error {
	if err := c.verifyChannels(ctx, sink); err != nil {
		return err
	}
	target, err := c.openConnection(ctx)
	if err != nil {
		return err
	}
	ws, response, err := websocket.DefaultDialer.DialContext(ctx, target, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return fmt.Errorf("dialing slack socket mode: %w", err)
	}
	c.mu.Lock()
	c.conn = ws
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == ws {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = ws.Close()
	}()

	stop := make(chan struct{})
	done := make(chan struct{})
	alive := func() { _ = ws.SetReadDeadline(time.Now().Add(3 * keepalive)) }
	alive()
	ws.SetPongHandler(func(string) error { alive(); return nil })
	ws.SetPingHandler(func(data string) error {
		alive()
		err := ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		return err
	})
	go func() {
		defer close(done)
		ticker := time.NewTicker(keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = ws.Close()
				return
			case <-stop:
				return
			case <-ticker.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
					_ = ws.Close()
					return
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()

	var writeMu sync.Mutex
	ack := func(id string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(map[string]string{"envelope_id": id})
	}
	hello := false
	for {
		var e envelope
		if err := ws.ReadJSON(&e); err != nil {
			return fmt.Errorf("reading slack socket mode: %w", err)
		}
		alive()
		switch e.Type {
		case "hello":
			hello = true
			c.connected()
		case "disconnect":
			if e.Reason == "link_disabled" {
				c.setHealth(connector.HealthFailed, "Socket Mode is turned off for the Slack app")
				return fmt.Errorf("%w: slack disabled the socket mode link", connector.ErrStreamPermanent)
			}
			if !hello {
				return fmt.Errorf("slack closed the connection before hello: %s", e.Reason)
			}
			return errRefresh
		case "events_api":
			if !hello {
				return errors.New("slack sent an event before hello")
			}
			// The envelope is acknowledged once what it carries is through
			// the gate. An emit that fails leaves it unacknowledged, and Slack
			// delivers it again.
			if err := c.dispatch(ctx, sink, e.Payload); err != nil {
				return err
			}
			if err := ack(e.EnvelopeID); err != nil {
				return fmt.Errorf("acknowledging a slack event: %w", err)
			}
		}
		// Slash commands and interactions are not answered in this version:
		// Slack tells the person that nothing handled them.
	}
}

// connected records a hello: ok, or degraded where a configured channel does
// not have the app in it and so sends nothing.
func (c *Connector) connected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.notMember) > 0 {
		c.status = connector.HealthDegraded
		c.detail = "connected; the app is not a member of " + strings.Join(c.notMember, ", ") + ", so Slack sends nothing from it: invite the app"
		return
	}
	c.status = connector.HealthOK
	c.detail = "connected"
}

// apiError is a Web API call Slack answered with ok: false.
type apiError struct {
	method, code string
}

func (e *apiError) Error() string { return "slack " + e.method + ": " + e.code }

// permanent reports an answer no retry changes: the token or its scopes are
// wrong, or the channel is not one the token can see.
func (e *apiError) permanent() bool {
	switch e.code {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired",
		"not_allowed_token_type", "missing_scope", "no_permission", "channel_not_found", "team_access_not_granted":
		return true
	}
	return false
}

// call makes one Web API request and decodes its answer into out, which
// embeds [response].
func (c *Connector) call(ctx context.Context, method, token string, form url.Values, out interface{ result() response }) error {
	c.mu.Lock()
	delay := time.Until(c.nextRequest)
	c.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		seconds, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s")
		if err != nil || seconds <= 0 {
			seconds = time.Second
		}
		c.mu.Lock()
		if until := time.Now().Add(seconds); until.After(c.nextRequest) {
			c.nextRequest = until
		}
		c.mu.Unlock()
		return fmt.Errorf("slack %s: rate limited", method)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("slack %s: reading the answer: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("slack %s: decoding the answer: %w", method, err)
	}
	if r := out.result(); !r.OK {
		return &apiError{method: method, code: r.Error}
	}
	return nil
}

// fail turns a Web API refusal no retry can change into failed health and a
// permanent stream error.
func (c *Connector) fail(err error, detail string) error {
	var api *apiError
	if errors.As(err, &api) && api.permanent() {
		c.setHealth(connector.HealthFailed, detail+": "+api.code)
		return fmt.Errorf("%w: %w", connector.ErrStreamPermanent, err)
	}
	return err
}

type response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (r response) result() response { return r }

// openConnection asks Slack for a Socket Mode URL with the app-level token.
func (c *Connector) openConnection(ctx context.Context) (string, error) {
	var r struct {
		response
		URL string `json:"url"`
	}
	if err := c.call(ctx, "apps.connections.open", c.appToken, nil, &r); err != nil {
		return "", c.fail(err, "Slack refused the app-level token")
	}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		return "", errors.New("slack apps.connections.open returned no WebSocket URL")
	}
	return r.URL, nil
}

// channelInfo is what conversations.info says about a channel's class.
type channelInfo struct {
	ID                 string `json:"id"`
	IsChannel          bool   `json:"is_channel"`
	IsGroup            bool   `json:"is_group"`
	IsIM               bool   `json:"is_im"`
	IsMPIM             bool   `json:"is_mpim"`
	IsPrivate          bool   `json:"is_private"`
	IsExtShared        bool   `json:"is_ext_shared"`
	IsPendingExtShared bool   `json:"is_pending_ext_shared"`
	IsMember           bool   `json:"is_member"`
	IsArchived         bool   `json:"is_archived"`
}

// unsupported says why a channel is not one this version ingests, or "".
func (ch channelInfo) unsupported() string {
	switch {
	case ch.IsIM:
		return "is a direct message"
	case ch.IsMPIM:
		return "is a group direct message"
	case ch.IsGroup:
		return "is a private channel"
	case ch.IsExtShared || ch.IsPendingExtShared:
		return "is shared with another organisation through Slack Connect"
	case !ch.IsChannel:
		return "is not a channel"
	}
	return ""
}

// verifyChannels reads every configured channel with the bot token. A channel
// of a class this version does not ingest is a configuration error, so it
// stops the source; one the app is not in is remembered for health.
func (c *Connector) verifyChannels(ctx context.Context, sink connector.Sink) error {
	var notMember []string
	for _, id := range c.containers {
		var r struct {
			response
			Channel channelInfo `json:"channel"`
		}
		if err := c.call(ctx, "conversations.info", c.botToken, url.Values{"channel": {id}}, &r); err != nil {
			return c.fail(err, "Slack refused to describe channel "+id)
		}
		if why := r.Channel.unsupported(); why != "" {
			c.setHealth(connector.HealthFailed, "channel "+id+" "+why+": only configured public-channel ids are supported, so remove it from containers")
			return fmt.Errorf("%w: slack channel %s %s", connector.ErrStreamPermanent, id, why)
		}
		if !r.Channel.IsMember {
			notMember = append(notMember, id)
		}
		if err := c.observeVisibility(ctx, sink, id, r.Channel.IsPrivate || r.Channel.IsArchived); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.notMember = notMember
	c.mu.Unlock()
	return nil
}

// Package discord ingests live guild messages, threads, and reactions through
// the Gateway and walks channel and thread history through REST. A source uses
// settings `guild` (the guild snowflake),
// `intents` (Gateway bitset; GUILDS, GUILD_MESSAGES, GUILD_MESSAGE_REACTIONS,
// and MESSAGE_CONTENT are needed), and optionally `gateway_url` and `api_url`
// for local fixtures. Its only secret is `token`, the bot token. The runtime resolves
// that secret from the environment before calling Factory. A source's
// containers are parent channel snowflakes; the runtime Gate enforces them.
// Discord's REST lists expose current messages but no deleted-message list.
//
// Slash commands are not the connector's. `/hearsay pin` and `/hearsay merge`
// are answered by the API runtime's interaction adapter (ADR-0022) through
// [App], for a source whose settings also name `application_id` and
// `public_key` (the application's hex Ed25519 key, from the developer
// portal). Invite the bot with the `applications.commands` OAuth2 scope as
// well as `bot`, and set the application's Interactions Endpoint URL to the
// API's `/discord/<source id>/interactions`. At startup the API registers the
// commands in the guild with the bot token. It verifies every interaction
// with the public key, records each command as an L0 `command` event, and
// answers it ephemerally with the interaction's own token, within Discord's
// three seconds or through a deferred answer it edits later. It never posts a
// channel message. A source without the two settings takes no commands.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
)

const (
	// Type is the connector type registered by the binary.
	Type = "discord"
	// SecretToken is the name of the bot credential in source secrets.
	SecretToken     = "token"
	defaultGateway  = "wss://gateway.discord.gg/?v=10&encoding=json"
	defaultAPI      = "https://discord.com/api/v10"
	requiredIntents = (1 << 0) | (1 << 9) | (1 << 10) | (1 << 15)
	viewChannel     = uint64(1 << 10)
)

// Settings configures a guild Gateway stream.
type Settings struct {
	Guild      string `json:"guild"`
	Intents    int    `json:"intents"`
	GatewayURL string `json:"gateway_url"`
	APIURL     string `json:"api_url"`
	// ApplicationID and PublicKey name the Discord application whose slash
	// commands the API answers ([App]). The connector does not use them.
	ApplicationID string `json:"application_id"`
	PublicKey     string `json:"public_key"`
}

// Connector maintains one Discord Gateway session for a source.
type Connector struct {
	source, guild, token, gateway string
	intents                       int
	api                           string
	http                          *http.Client
	containers                    []string
	nextRequest                   time.Time
	mu                            sync.Mutex
	status                        connector.HealthStatus
	detail                        string
	last                          time.Time
	session, resumeURL            string
	seq                           int64
	conn                          *websocket.Conn
	stop                          context.CancelFunc
	active                        chan struct{}
	closed                        bool
	channels                      map[string]channel
	roles                         map[string]uint64
	messages                      map[string]message
}

var (
	_ connector.Streamer   = (*Connector)(nil)
	_ connector.Backfiller = (*Connector)(nil)
	_ connector.Resyncer   = (*Connector)(nil)
)

// Factory builds a Discord connector for the runtime registry.
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New validates a resolved source and creates an idle connector.
func New(src connector.SourceConfig) (*Connector, error) {
	var s Settings
	if err := src.DecodeSettings(&s); err != nil {
		return nil, err
	}
	if !snowflake(s.Guild) {
		return nil, errors.New("discord setting guild must be a snowflake")
	}
	if _, _, err := s.app(); err != nil {
		return nil, err
	}
	if s.Intents == 0 {
		s.Intents = requiredIntents
	}
	if s.Intents&requiredIntents != requiredIntents {
		return nil, errors.New("discord intents must include GUILDS, GUILD_MESSAGES, GUILD_MESSAGE_REACTIONS, and MESSAGE_CONTENT")
	}
	for name := range src.Secrets {
		if name != SecretToken {
			return nil, fmt.Errorf("discord does not read secret %q", name)
		}
	}
	if src.Secrets[SecretToken] == "" {
		return nil, errors.New("discord secret token is required")
	}
	if len(src.Containers) == 0 {
		return nil, errors.New("discord containers must name at least one channel")
	}
	for _, id := range src.Containers {
		if id != connector.AllowAll && !snowflake(id) {
			return nil, fmt.Errorf("discord container %q must be a channel snowflake", id)
		}
	}
	gateway := s.GatewayURL
	if gateway == "" {
		gateway = defaultGateway
	}
	u, err := url.Parse(gateway)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		return nil, errors.New("discord gateway_url must be a WebSocket URL")
	}
	if u.Scheme == "ws" && !strings.HasPrefix(u.Host, "127.0.0.1:") && !strings.HasPrefix(u.Host, "localhost:") {
		return nil, errors.New("discord gateway_url must use wss outside localhost")
	}
	api, err := apiURL(s.APIURL)
	if err != nil {
		return nil, err
	}
	containers := slices.Clone(src.Containers)
	if slices.Contains(containers, connector.AllowAll) {
		return nil, errors.New("discord containers must name channel snowflakes, not *")
	}
	slices.Sort(containers)
	return &Connector{source: src.ID, guild: s.Guild, token: src.Secrets[SecretToken], gateway: gateway, api: api, http: &http.Client{Timeout: 30 * time.Second}, containers: containers, intents: s.Intents, status: connector.HealthDegraded, detail: "reconnecting", channels: map[string]channel{}, roles: map[string]uint64{}, messages: map[string]message{}}, nil
}

// apiURL is the REST base a source's settings name, without a trailing slash.
func apiURL(api string) (string, error) {
	if api == "" {
		api = defaultAPI
	}
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("discord api_url must be an HTTP URL without query or fragment")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		return "", errors.New("discord api_url must use https outside localhost")
	}
	return strings.TrimRight(api, "/"), nil
}

func snowflake(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Describe declares the kinds the Gateway emits.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindMessage, connector.KindThread, connector.KindReaction, connector.KindTombstone}}
}

// Health reports cached connection state without network IO.
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

// Close stops the socket and waits for Stream's goroutines to leave.
func (c *Connector) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	conn := c.conn
	active := c.active
	stop := c.stop
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

type envelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}
type hello struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}
type ready struct {
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
}

// Stream connects, identifies or resumes, and consumes Gateway dispatches.
func (c *Connector) Stream(ctx context.Context, sink connector.Sink) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return connector.ErrClosed
	}
	if c.active != nil {
		c.mu.Unlock()
		return errors.New("discord stream already running")
	}
	active := make(chan struct{})
	c.active = active
	ctx, cancelStream := context.WithCancel(ctx)
	c.stop = cancelStream
	if c.status != connector.HealthFailed {
		c.status = connector.HealthDegraded
		c.detail = "reconnecting"
	}
	target := c.gateway
	if c.session != "" && c.resumeURL != "" {
		target = c.resumeURL
	}
	c.mu.Unlock()
	defer func() {
		cancelStream()
		c.mu.Lock()
		c.active = nil
		c.stop = nil
		c.mu.Unlock()
		close(active)
	}()
	ws, response, err := websocket.DefaultDialer.DialContext(ctx, target, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return fmt.Errorf("dial discord gateway: %w", err)
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
		if ctx.Err() == nil {
			c.mu.Lock()
			if c.status != connector.HealthFailed {
				c.status = connector.HealthDegraded
				c.detail = "reconnecting"
			}
			c.mu.Unlock()
		}
	}()
	stop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-stop:
		}
	}()
	defer func() { close(stop); <-watchDone }()
	var first envelope
	if err := ws.ReadJSON(&first); err != nil {
		return fmt.Errorf("read gateway hello: %w", err)
	}
	if first.Op != 10 {
		return errors.New("gateway did not send HELLO")
	}
	var h hello
	if err := json.Unmarshal(first.D, &h); err != nil || h.HeartbeatInterval <= 0 {
		return errors.New("gateway HELLO has invalid heartbeat interval")
	}
	var writeMu sync.Mutex
	send := func(op int, d any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(map[string]any{"op": op, "d": d})
	}
	c.mu.Lock()
	session, seq := c.session, c.seq
	c.mu.Unlock()
	if session != "" {
		err = send(6, map[string]any{"token": c.token, "session_id": session, "seq": seq})
	} else {
		err = send(2, map[string]any{"token": c.token, "intents": c.intents, "properties": map[string]string{"os": "hearsay", "browser": "hearsay", "device": "hearsay"}})
	}
	if err != nil {
		return fmt.Errorf("identify or resume: %w", err)
	}
	hbCtx, cancel := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	ack := make(chan struct{}, 1)
	ack <- struct{}{}
	go func() {
		defer close(hbDone)
		timer := time.NewTimer(time.Duration(rand.Float64()*float64(h.HeartbeatInterval)) * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-timer.C:
				select {
				case <-ack:
				default:
					_ = ws.Close()
					return
				}
				c.mu.Lock()
				seq := c.seq
				c.mu.Unlock()
				if send(1, seq) != nil {
					_ = ws.Close()
					return
				}
				timer.Reset(time.Duration(h.HeartbeatInterval) * time.Millisecond)
			}
		}
	}()
	defer func() { cancel(); <-hbDone }()
	for {
		var e envelope
		if err := ws.ReadJSON(&e); err != nil {
			var closed *websocket.CloseError
			if errors.As(err, &closed) {
				switch closed.Code {
				case 4007, 4009:
					c.mu.Lock()
					c.session = ""
					c.resumeURL = ""
					c.seq = 0
					c.mu.Unlock()
				case 4004, 4010, 4011, 4012, 4013, 4014:
					c.setHealth(connector.HealthFailed, "gateway rejected bot token, intents, or configuration")
					return fmt.Errorf("%w: gateway close code %d", connector.ErrStreamPermanent, closed.Code)
				}
			}
			return fmt.Errorf("read discord gateway: %w", err)
		}
		switch e.Op {
		case 0:
			if e.T == "READY" {
				var v ready
				if err := json.Unmarshal(e.D, &v); err != nil {
					return err
				}
				c.mu.Lock()
				c.session = v.SessionID
				c.resumeURL = v.ResumeGatewayURL
				c.mu.Unlock()
				c.setHealth(connector.HealthOK, "connected")
				// A fresh session has no replay sequence. A durable walk fills
				// messages posted while this process was down, even after the
				// one-time initial backfill finished in an earlier run.
				if requester, ok := sink.(connector.ResyncRequester); ok {
					for _, id := range c.containers {
						if err := requester.RequestResync(ctx, id); err != nil && !errors.Is(err, connector.ErrNoResyncStore) {
							return fmt.Errorf("recording restart gap walk for channel %s: %w", id, err)
						}
					}
				}
			}
			if e.T == "RESUMED" {
				c.setHealth(connector.HealthOK, "connected")
			}
			if err := c.dispatch(ctx, sink, e.T, e.D); err != nil {
				return err
			}
			if e.S != nil {
				c.mu.Lock()
				c.seq = *e.S
				c.mu.Unlock()
			}
		case 1:
			c.mu.Lock()
			seq := c.seq
			c.mu.Unlock()
			if err := send(1, seq); err != nil {
				return err
			}
		case 7:
			return errors.New("gateway requested reconnect")
		case 9:
			var resumable bool
			_ = json.Unmarshal(e.D, &resumable)
			if !resumable {
				c.mu.Lock()
				c.session = ""
				c.resumeURL = ""
				c.seq = 0
				c.mu.Unlock()
			}
			return errors.New("gateway invalidated session")
		case 11:
			select {
			case ack <- struct{}{}:
			default:
			}
		}
	}
}

// Discord snowflakes encode creation time in their high 42 bits.
func snowflakeTime(id string) time.Time {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + 1420070400000).UTC()
}

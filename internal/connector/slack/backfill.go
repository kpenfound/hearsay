package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

const historyPage = "100"

type walkPosition struct {
	Channel     string   `json:"channel"`
	Cursor      string   `json:"cursor,omitempty"`
	Pending     []string `json:"pending,omitempty"`
	Thread      string   `json:"thread,omitempty"`
	ReplyCursor string   `json:"reply_cursor,omitempty"`
	// Next is the history cursor to use once this page's threads are walked.
	Next string `json:"next,omitempty"`
	End  bool   `json:"end,omitempty"`
}

func (c *Connector) configured(id string) bool { return slices.Contains(c.containers, id) }

func parsePosition(from connector.Cursor) (walkPosition, error) {
	var p walkPosition
	if err := json.Unmarshal([]byte(from), &p); err != nil {
		return p, fmt.Errorf("slack cursor: %w", err)
	}
	if !slackID(p.Channel, 'C') || len(from) > connector.MaxCursorLen {
		return p, errors.New("invalid slack cursor")
	}
	return p, nil
}

func position(p walkPosition, count int) (connector.BackfillResult, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	if len(raw) > connector.MaxCursorLen {
		return connector.BackfillResult{}, errors.New("slack cursor exceeds runtime limit")
	}
	return connector.BackfillResult{Next: connector.Cursor(raw), Events: count}, nil
}

// Backfill walks one bounded history or reply page from a durable cursor.
func (c *Connector) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	p := walkPosition{Channel: c.containers[0]}
	if from != "" {
		var err error
		p, err = parsePosition(from)
		if err != nil {
			return connector.BackfillResult{}, err
		}
		if !c.configured(p.Channel) {
			i := 0
			for i < len(c.containers) && c.containers[i] <= p.Channel {
				i++
			}
			if i == len(c.containers) {
				return connector.BackfillResult{Done: true}, nil
			}
			p = walkPosition{Channel: c.containers[i]}
		}
	}
	return c.walk(ctx, sink, p)
}

func (c *Connector) walk(ctx context.Context, sink connector.Sink, p walkPosition) (connector.BackfillResult, error) {
	public, err := c.Public(ctx, p.Channel)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	if !public {
		requester, ok := sink.(connector.ResyncRequester)
		if !ok {
			return connector.BackfillResult{}, connector.ErrNoResyncStore
		}
		if err := requester.RequestResync(ctx, p.Channel); err != nil {
			return connector.BackfillResult{}, err
		}
		return c.nextChannel(p)
	}
	if p.Thread != "" || len(p.Pending) > 0 {
		if p.Thread == "" {
			p.Thread, p.Pending = p.Pending[0], p.Pending[1:]
		}
		q := url.Values{"channel": {p.Channel}, "ts": {p.Thread}, "limit": {historyPage}}
		if p.ReplyCursor != "" {
			q.Set("cursor", p.ReplyCursor)
		}
		page, err := c.messages(ctx, "conversations.replies", q)
		if err != nil {
			return connector.BackfillResult{}, err
		}
		for _, m := range page.Messages {
			m.Channel = p.Channel
			if m.TS != p.Thread && m.ThreadTS == "" {
				m.ThreadTS = p.Thread
			}
			if content[m.Subtype] {
				if err := c.emitMessage(ctx, sink, m); err != nil {
					return connector.BackfillResult{}, err
				}
			}
		}
		if page.Next != "" && page.Next == p.ReplyCursor {
			return connector.BackfillResult{}, errors.New("slack replies page made no progress")
		}
		p.ReplyCursor = page.Next
		if page.Next == "" {
			p.Thread = ""
			p.ReplyCursor = ""
			if len(p.Pending) == 0 && p.End {
				return c.nextChannel(p)
			}
		}
		return position(p, len(page.Messages))
	}
	if p.Next != "" {
		p.Cursor, p.Next = p.Next, ""
	}
	q := url.Values{"channel": {p.Channel}, "limit": {historyPage}}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	page, err := c.messages(ctx, "conversations.history", q)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	for _, m := range page.Messages {
		m.Channel = p.Channel
		if content[m.Subtype] {
			if err := c.emitMessage(ctx, sink, m); err != nil {
				return connector.BackfillResult{}, err
			}
		}
		if m.ReplyCount > 0 && m.TS != "" {
			p.Pending = append(p.Pending, m.TS)
		}
	}
	if page.Next != "" && page.Next == p.Cursor {
		return connector.BackfillResult{}, errors.New("slack history page made no progress")
	}
	p.Next = page.Next
	p.End = page.Next == ""
	if len(p.Pending) == 0 {
		if p.End {
			return c.nextChannel(p)
		}
		p.Cursor, p.Next = p.Next, ""
	}
	return position(p, len(page.Messages))
}

func (c *Connector) nextChannel(p walkPosition) (connector.BackfillResult, error) {
	i := slices.Index(c.containers, p.Channel) + 1
	if i >= len(c.containers) {
		return connector.BackfillResult{Done: true}, nil
	}
	return position(walkPosition{Channel: c.containers[i]}, 0)
}

type messagePage struct {
	response
	Messages []message `json:"messages"`
	Metadata struct {
		Next string `json:"next_cursor"`
	} `json:"response_metadata"`
	Next string `json:"-"`
}

func (p *messagePage) result() response { return p.response }
func (c *Connector) messages(ctx context.Context, method string, q url.Values) (messagePage, error) {
	var p messagePage
	if err := c.call(ctx, method, c.botToken, q, &p); err != nil {
		return p, err
	}
	p.Next = p.Metadata.Next
	return p, nil
}

// Public is queried by the runtime at startup for previously exposed channels.
func (c *Connector) Public(ctx context.Context, container string) (bool, error) {
	if !c.configured(container) {
		return false, fmt.Errorf("slack channel %s is not configured", container)
	}
	var r struct {
		response
		Channel channelInfo `json:"channel"`
	}
	err := c.call(ctx, "conversations.info", c.botToken, url.Values{"channel": {container}}, &r)
	if err != nil {
		var api *apiError
		if errors.As(err, &api) && (api.code == "channel_not_found" || api.code == "not_in_channel") {
			return false, nil
		}
		return false, err
	}
	if r.Channel.ID != container {
		return false, errors.New("slack returned a different channel")
	}
	return r.Channel.IsChannel && !r.Channel.IsPrivate && !r.Channel.IsArchived && !r.Channel.IsExtShared && !r.Channel.IsPendingExtShared, nil
}

// observeVisibility records an obligation before a Socket Mode envelope is acked.
func (c *Connector) observeVisibility(ctx context.Context, sink connector.Sink, channel string, restricted bool) error {
	c.mu.Lock()
	old, seen := c.restricted[channel]
	c.mu.Unlock()
	if restricted && seen && !old {
		requester, ok := sink.(connector.ResyncRequester)
		if !ok {
			return connector.ErrNoResyncStore
		}
		if err := requester.RequestResync(ctx, channel); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.restricted[channel] = restricted
	c.mu.Unlock()
	return nil
}

// Resync walks current L0 artifacts rather than Slack history: after a channel
// becomes private the bot may lose access to conversations.history entirely.
// The artifact key is a stable, restart-safe position in the L0 snapshot.
func (c *Connector) Resync(ctx context.Context, sink connector.Sink, container string, from connector.Cursor) (connector.BackfillResult, error) {
	if !c.configured(container) {
		return connector.BackfillResult{}, fmt.Errorf("slack channel %s is not configured", container)
	}
	reader, ok := sink.(connector.ArtifactReader)
	if !ok {
		return connector.BackfillResult{}, errors.New("slack re-sync requires current artifacts")
	}
	current, err := reader.CurrentArtifacts(ctx, c.source)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	n := 0
	for _, ev := range current {
		if ev.Payload.Container.NativeID != container || ev.Payload.Artifact <= string(from) {
			continue
		}
		if ev.ACL[0].Kind == connector.ACLPublic {
			ev.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: c.source, NativeID: container}}
			token := "perm:private"
			var editedAt time.Time
			if ev.Payload.Revision != nil {
				token = strings.TrimSuffix(ev.Payload.Revision.Token, "+perm:public") + "+perm:private"
				editedAt = ev.Payload.Revision.EditedAt
			}
			ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: editedAt}
			ev.NativeID = ev.Payload.Artifact + "@" + token
			if err := c.emit(ctx, sink, ev); err != nil {
				return connector.BackfillResult{}, err
			}
		}
		n++
		if n == 100 {
			return connector.BackfillResult{Next: connector.Cursor(ev.Payload.Artifact), Events: n}, nil
		}
	}
	return connector.BackfillResult{Done: true, Events: n}, nil
}

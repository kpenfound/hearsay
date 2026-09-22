package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

const messagePage = 100

// walkPosition is deliberately self-contained: the runtime persists it between
// calls, and no page relies on the connector's in-memory gateway cache.
type walkPosition struct {
	Channel       string   `json:"channel"`
	Phase         string   `json:"phase"`
	Target        string   `json:"target,omitempty"`
	Before        string   `json:"before,omitempty"`
	After         string   `json:"after,omitempty"`
	Pending       []string `json:"pending,omitempty"`
	ArchiveBefore string   `json:"archive_before,omitempty"`
	More          bool     `json:"more,omitempty"`
}

// Backfill reads one bounded page of allowlisted channel or thread history.
func (c *Connector) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	if len(c.containers) == 0 {
		return connector.BackfillResult{Done: true}, nil
	}
	p := walkPosition{Channel: c.containers[0], Phase: "messages"}
	if from != "" {
		var err error
		p, err = parsePosition(from)
		if err != nil {
			return connector.BackfillResult{}, err
		}
		if !slices.Contains(c.containers, p.Channel) {
			i := 0
			for i < len(c.containers) && c.containers[i] <= p.Channel {
				i++
			}
			if i == len(c.containers) {
				return connector.BackfillResult{Done: true}, nil
			}
			p = walkPosition{Channel: c.containers[i], Phase: "messages"}
		}
	}
	return c.walk(ctx, sink, p, false)
}

// Public asks Discord whether a configured parent channel is public now.
func (c *Connector) Public(ctx context.Context, container string) (bool, error) {
	if !slices.Contains(c.containers, container) {
		return false, fmt.Errorf("discord channel %s is not configured", container)
	}
	if err := c.refresh(ctx, container, ""); err != nil {
		return false, err
	}
	_, _, acl := c.place(container)
	return len(acl) == 1 && acl[0].Kind == connector.ACLPublic, nil
}

// Resync walks one channel under its current permissions from a stored cursor.
func (c *Connector) Resync(ctx context.Context, sink connector.Sink, container string, from connector.Cursor) (connector.BackfillResult, error) {
	if !slices.Contains(c.containers, container) {
		return connector.BackfillResult{}, fmt.Errorf("discord channel %s is not configured", container)
	}
	p := walkPosition{Channel: container, Phase: "messages"}
	if from != "" {
		var err error
		p, err = parsePosition(from)
		if err != nil {
			return connector.BackfillResult{}, err
		}
		if p.Channel != container {
			return connector.BackfillResult{}, fmt.Errorf("discord re-sync cursor names %s, not %s", p.Channel, container)
		}
	}
	return c.walk(ctx, sink, p, true)
}

func parsePosition(from connector.Cursor) (walkPosition, error) {
	var p walkPosition
	if err := json.Unmarshal([]byte(from), &p); err != nil {
		return p, fmt.Errorf("discord cursor: %w", err)
	}
	if !snowflake(p.Channel) || !slices.Contains([]string{"messages", "active", "public_archived", "private_archived"}, p.Phase) || (p.Target != "" && !snowflake(p.Target)) || (p.Before != "" && !snowflake(p.Before)) || (p.After != "" && !snowflake(p.After)) {
		return p, errors.New("invalid discord cursor")
	}
	for _, id := range p.Pending {
		if !snowflake(id) {
			return p, errors.New("invalid discord cursor thread")
		}
	}
	return p, nil
}

func (c *Connector) result(p walkPosition, resync bool, n int) (connector.BackfillResult, error) {
	if p.Phase == "done" {
		if resync {
			return connector.BackfillResult{Done: true, Events: n}, nil
		}
		i := slices.Index(c.containers, p.Channel) + 1
		if i >= len(c.containers) {
			return connector.BackfillResult{Done: true, Events: n}, nil
		}
		p = walkPosition{Channel: c.containers[i], Phase: "messages"}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	if len(raw) > connector.MaxCursorLen {
		return connector.BackfillResult{}, errors.New("discord cursor exceeds runtime limit")
	}
	return connector.BackfillResult{Next: connector.Cursor(raw), Events: n}, nil
}

// Each call reads at most one message page or one thread listing. Message
// pages use snowflake keyset pagination, so new messages cannot shift history
// across a page boundary.
func (c *Connector) walk(ctx context.Context, sink connector.Sink, p walkPosition, resync bool) (connector.BackfillResult, error) {
	if p.Target != "" || p.Phase == "messages" {
		id := p.Channel
		if p.Target != "" {
			id = p.Target
		}
		if err := c.refresh(ctx, p.Channel, p.Target); err != nil {
			return connector.BackfillResult{}, err
		}
		if p.Target != "" && p.Before == "" {
			c.mu.Lock()
			th := c.channels[p.Target]
			c.mu.Unlock()
			if err := c.emitThread(ctx, sink, th); err != nil {
				return connector.BackfillResult{}, err
			}
		}
		q := url.Values{"limit": {strconv.Itoa(messagePage)}}
		if p.Before != "" {
			q.Set("before", p.Before)
		}
		var msgs []message
		if err := c.get(ctx, "/channels/"+id+"/messages?"+q.Encode(), &msgs); err != nil {
			return connector.BackfillResult{}, err
		}
		for _, m := range msgs {
			// REST's channel id is authoritative, even if a fixture omits it.
			if m.ChannelID == "" {
				m.ChannelID = id
			}
			if err := c.emitMessage(ctx, sink, m); err != nil {
				return connector.BackfillResult{}, err
			}
		}
		n := len(msgs)
		if n == messagePage {
			oldest := msgs[n-1].ID
			if !snowflake(oldest) || oldest == p.Before {
				return connector.BackfillResult{}, errors.New("discord message page made no progress")
			}
			p.Before = oldest
		} else {
			p.Before = ""
			if p.Target != "" {
				p.Target = ""
			} else {
				p.Phase = "active"
			}
		}
		return c.result(p, resync, n)
	}
	if p.Phase == "active" {
		var listing struct {
			Threads []channel `json:"threads"`
		}
		if err := c.get(ctx, "/guilds/"+c.guild+"/threads/active", &listing); err != nil {
			return connector.BackfillResult{}, err
		}
		var ids []string
		for _, ch := range listing.Threads {
			if ch.ParentID == p.Channel && ch.ID > p.After {
				ids = append(ids, ch.ID)
			}
		}
		slices.Sort(ids)
		if len(ids) == 0 {
			p.Phase = "public_archived"
			p.After = ""
		} else {
			p.Target = ids[0]
			p.After = ids[0]
		}
		return c.result(p, resync, 0)
	}
	if len(p.Pending) == 0 && p.More {
		p.More = false
	} // consume a completed archive page
	if len(p.Pending) == 0 {
		if p.ArchiveBefore == "done" {
			if p.Phase == "public_archived" {
				p.Phase = "private_archived"
				p.ArchiveBefore = ""
			} else {
				p.Phase = "done"
			}
			return c.result(p, resync, 0)
		}
		visibility := "public"
		if p.Phase == "private_archived" {
			visibility = "private"
		}
		q := url.Values{"limit": {"50"}}
		if p.ArchiveBefore != "" {
			q.Set("before", p.ArchiveBefore)
		}
		var listing struct {
			Threads []struct {
				channel
				ThreadMetadata struct {
					ArchiveTimestamp string `json:"archive_timestamp"`
				} `json:"thread_metadata"`
			} `json:"threads"`
			HasMore bool `json:"has_more"`
		}
		if err := c.get(ctx, "/channels/"+p.Channel+"/threads/archived/"+visibility+"?"+q.Encode(), &listing); err != nil {
			return connector.BackfillResult{}, err
		}
		for _, ch := range listing.Threads {
			p.Pending = append(p.Pending, ch.ID)
		}
		if len(listing.Threads) > 0 {
			last := listing.Threads[len(listing.Threads)-1]
			before := last.ThreadMetadata.ArchiveTimestamp
			if visibility == "private" {
				before = last.ID
			}
			if listing.HasMore && (before == "" || before == p.ArchiveBefore) {
				return connector.BackfillResult{}, errors.New("discord archived thread page made no progress")
			}
			p.ArchiveBefore = before
		}
		if !listing.HasMore {
			p.More = false
			if len(p.Pending) == 0 {
				p.ArchiveBefore = "done"
			}
		} else {
			p.More = true
		}
		if len(p.Pending) == 0 && listing.HasMore {
			return connector.BackfillResult{}, errors.New("discord archived thread page was empty but has_more")
		}
		return c.result(p, resync, 0)
	}
	p.Target = p.Pending[0]
	p.Pending = p.Pending[1:]
	if len(p.Pending) == 0 && !p.More {
		p.ArchiveBefore = "done"
	}
	return c.result(p, resync, 0)
}

// refresh reads current visibility before each page. A gateway and a REST
// walk may run together; both update the same cache under the connector lock.
func (c *Connector) refresh(ctx context.Context, parent, thread string) error {
	var roles []role
	if err := c.get(ctx, "/guilds/"+c.guild+"/roles", &roles); err != nil {
		return err
	}
	var ch channel
	if err := c.get(ctx, "/channels/"+parent, &ch); err != nil {
		return err
	}
	if ch.ID != parent {
		return errors.New("discord returned a different parent channel")
	}
	var th channel
	if thread != "" {
		if err := c.get(ctx, "/channels/"+thread, &th); err != nil {
			return err
		}
		if th.ID != thread || th.ParentID != parent {
			return errors.New("discord returned a thread outside its parent")
		}
	}
	c.mu.Lock()
	for _, r := range roles {
		bits, _ := strconv.ParseUint(r.Permissions, 10, 64)
		c.roles[r.ID] = bits
	}
	c.channels[parent] = ch
	if thread != "" {
		c.channels[thread] = th
	}
	c.mu.Unlock()
	return nil
}

func (c *Connector) get(ctx context.Context, path string, v any) error {
	// A 429 sets the next permitted request time for all concurrent walks.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("User-Agent", "hearsay")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("discord GET %s: %w", strings.Split(path, "?")[0], err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		var limited struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&limited)
		delay := time.Duration(limited.RetryAfter * float64(time.Second))
		if delay <= 0 {
			delay = time.Second
		}
		c.mu.Lock()
		until := time.Now().Add(delay)
		if until.After(c.nextRequest) {
			c.nextRequest = until
		}
		c.mu.Unlock()
		return fmt.Errorf("discord GET %s: rate limited", strings.Split(path, "?")[0])
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discord GET %s: %d %s", strings.Split(path, "?")[0], resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v); err != nil {
		return fmt.Errorf("decode discord GET %s: %w", strings.Split(path, "?")[0], err)
	}
	return nil
}

package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

type overwrite struct {
	ID    string `json:"id"`
	Type  int    `json:"type"`
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}
type channel struct {
	ID                   string      `json:"id"`
	GuildID              string      `json:"guild_id"`
	ParentID             string      `json:"parent_id"`
	Name                 string      `json:"name"`
	Type                 int         `json:"type"`
	OwnerID              string      `json:"owner_id"`
	PermissionOverwrites []overwrite `json:"permission_overwrites"`
}
type role struct {
	ID          string `json:"id"`
	Permissions string `json:"permissions"`
}
type guild struct {
	ID       string    `json:"id"`
	Roles    []role    `json:"roles"`
	Channels []channel `json:"channels"`
	Threads  []channel `json:"threads"`
}
type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}
type member struct {
	Nick string `json:"nick"`
	User *user  `json:"user"`
}
type reference struct {
	MessageID string `json:"message_id"`
}
type message struct {
	ID               string          `json:"id"`
	GuildID          string          `json:"guild_id"`
	ChannelID        string          `json:"channel_id"`
	Author           *user           `json:"author"`
	Member           *member         `json:"member"`
	Content          string          `json:"content"`
	Timestamp        time.Time       `json:"timestamp"`
	EditedTimestamp  json.RawMessage `json:"edited_timestamp"`
	MessageReference *reference      `json:"message_reference"`
}
type reaction struct {
	GuildID   string  `json:"guild_id"`
	ChannelID string  `json:"channel_id"`
	MessageID string  `json:"message_id"`
	UserID    string  `json:"user_id"`
	Member    *member `json:"member"`
	Emoji     struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"emoji"`
}
type deleted struct {
	ID        string `json:"id"`
	GuildID   string `json:"guild_id"`
	ChannelID string `json:"channel_id"`
}
type bulkDeleted struct {
	IDs       []string `json:"ids"`
	GuildID   string   `json:"guild_id"`
	ChannelID string   `json:"channel_id"`
}

func (c *Connector) dispatch(ctx context.Context, sink connector.Sink, kind string, raw json.RawMessage) error {
	switch kind {
	case "GUILD_CREATE", "GUILD_UPDATE":
		var g guild
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		if g.ID != c.guild {
			return nil
		}
		c.mu.Lock()
		for _, r := range g.Roles {
			p, _ := strconv.ParseUint(r.Permissions, 10, 64)
			c.roles[r.ID] = p
		}
		for _, ch := range g.Channels {
			c.channels[ch.ID] = ch
		}
		for _, ch := range g.Threads {
			c.channels[ch.ID] = ch
		}
		c.mu.Unlock()
		for _, ch := range g.Threads {
			if err := c.emitThread(ctx, sink, ch); err != nil {
				return err
			}
		}
	case "CHANNEL_CREATE", "CHANNEL_UPDATE", "THREAD_CREATE", "THREAD_UPDATE":
		var ch channel
		if err := json.Unmarshal(raw, &ch); err != nil {
			return err
		}
		if ch.GuildID != c.guild {
			return nil
		}
		c.mu.Lock()
		c.channels[ch.ID] = ch
		c.mu.Unlock()
		if strings.HasPrefix(kind, "THREAD_") && kind == "THREAD_CREATE" {
			return c.emitThread(ctx, sink, ch)
		}
	case "THREAD_DELETE":
		var ch channel
		if err := json.Unmarshal(raw, &ch); err != nil {
			return err
		}
		if ch.GuildID != c.guild {
			return nil
		}
		c.mu.Lock()
		if ch.ParentID == "" {
			ch.ParentID = c.channels[ch.ID].ParentID
		}
		c.mu.Unlock()
		if ch.ParentID == "" {
			return nil
		}
		return c.emitTombstone(ctx, sink, ch.ID, ch.ID+":tombstone", ch.ID, snowflakeTime(ch.ID))
	case "MESSAGE_CREATE", "MESSAGE_UPDATE":
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if m.GuildID != c.guild {
			return nil
		}
		c.mu.Lock()
		if old, ok := c.messages[m.ID]; ok && kind == "MESSAGE_UPDATE" {
			if _, present := fields["content"]; !present {
				m.Content = old.Content
			}
			if _, present := fields["edited_timestamp"]; !present {
				m.EditedTimestamp = old.EditedTimestamp
			}
			if m.Author == nil {
				m.Author = old.Author
			}
			if m.Member == nil {
				m.Member = old.Member
			}
			if m.Timestamp.IsZero() {
				m.Timestamp = old.Timestamp
			}
			if m.MessageReference == nil {
				m.MessageReference = old.MessageReference
			}
			if m.ChannelID == "" {
				m.ChannelID = old.ChannelID
			}
		}
		c.messages[m.ID] = m
		c.mu.Unlock()
		if m.Author == nil {
			return nil
		} // a partial update without its create cannot supply an author
		return c.emitMessage(ctx, sink, m)
	case "MESSAGE_DELETE":
		var d deleted
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		if d.GuildID != c.guild {
			return nil
		}
		return c.emitTombstone(ctx, sink, d.ChannelID, d.ID+":tombstone", d.ID, snowflakeTime(d.ID))
	case "MESSAGE_DELETE_BULK":
		var d bulkDeleted
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		if d.GuildID != c.guild {
			return nil
		}
		for _, id := range d.IDs {
			if err := c.emitTombstone(ctx, sink, d.ChannelID, id+":tombstone", id, snowflakeTime(id)); err != nil {
				return err
			}
		}
	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE":
		var r reaction
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.GuildID != c.guild {
			return nil
		}
		return c.emitReaction(ctx, sink, r, kind == "MESSAGE_REACTION_REMOVE")
	}
	return nil
}

func (c *Connector) place(channelID string) (connector.Container, string, connector.ACL) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.channels[channelID]
	id := channelID
	thread := ""
	if ch.Type == 11 || ch.Type == 12 || ch.Type == 10 {
		thread = ch.ID
		if ch.ParentID != "" {
			id = ch.ParentID
		}
		ch = c.channels[id]
	}
	container := connector.Container{Kind: connector.ContainerChannel, NativeID: id, Name: ch.Name}
	public := c.roles[c.guild]&viewChannel != 0
	for _, ow := range ch.PermissionOverwrites {
		if ow.ID == c.guild && ow.Type == 0 {
			allow, _ := strconv.ParseUint(ow.Allow, 10, 64)
			deny, _ := strconv.ParseUint(ow.Deny, 10, 64)
			public = (public && deny&viewChannel == 0) || allow&viewChannel != 0
		}
	}
	// A private thread is narrower than its parent. It is read through its
	// own group, while the allowlist still names the parent channel.
	if c.channels[channelID].Type == 12 {
		return container, thread, connector.ACL{{Kind: connector.ACLGroup, Source: c.source, NativeID: channelID}}
	}
	if public {
		return container, thread, connector.ACL{{Kind: connector.ACLPublic}}
	}
	return container, thread, connector.ACL{{Kind: connector.ACLGroup, Source: c.source, NativeID: id}}
}
func (c *Connector) identity(u *user, nick string) *connector.Identity {
	if u == nil || u.ID == "" {
		return nil
	}
	kind := connector.IdentityUser
	if u.Bot {
		kind = connector.IdentityBot
	}
	display := nick
	if display == "" {
		display = u.GlobalName
	}
	if display == "" {
		display = u.Username
	}
	return &connector.Identity{Source: c.source, Kind: kind, NativeID: u.ID, Handle: u.Username, DisplayName: display}
}
func (c *Connector) event(kind connector.Kind, artifact, channelID string, at time.Time) connector.Event {
	container, thread, acl := c.place(channelID)
	return connector.Event{Source: c.source, NativeID: artifact, Kind: kind, Time: at, ACL: acl, Payload: connector.Payload{Artifact: artifact, Container: container, Thread: thread}}
}
func (c *Connector) emit(ctx context.Context, sink connector.Sink, ev connector.Event) error {
	if ev.Time.IsZero() {
		return fmt.Errorf("discord event %s has no source time", ev.NativeID)
	}
	if err := sink.Emit(ctx, ev); err != nil {
		return err
	}
	c.mu.Lock()
	c.last = time.Now().UTC()
	c.mu.Unlock()
	return nil
}
func (c *Connector) emitMessage(ctx context.Context, sink connector.Sink, m message) error {
	at := m.Timestamp
	if at.IsZero() {
		at = snowflakeTime(m.ID)
	}
	ev := c.event(connector.KindMessage, m.ID, m.ChannelID, at)
	ev.Payload.Text = m.Content
	nick := ""
	if m.Member != nil {
		nick = m.Member.Nick
	}
	ev.Payload.Author = c.identity(m.Author, nick)
	if m.MessageReference != nil {
		ev.Payload.Parent = m.MessageReference.MessageID
		if ev.Payload.Thread == "" {
			ev.Payload.Thread = m.MessageReference.MessageID
		}
	}
	ev.Payload.URL = permalink(c.guild, m.ChannelID, m.ID)
	if len(m.EditedTimestamp) > 0 && string(m.EditedTimestamp) != "null" {
		var token string
		if err := json.Unmarshal(m.EditedTimestamp, &token); err != nil {
			return err
		}
		edited, err := time.Parse(time.RFC3339Nano, token)
		if err != nil {
			return fmt.Errorf("discord edited_timestamp: %w", err)
		}
		ev.NativeID = m.ID + "@" + token
		ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: edited}
	}
	return c.emit(ctx, sink, ev)
}
func (c *Connector) emitThread(ctx context.Context, sink connector.Sink, ch channel) error {
	if ch.OwnerID == "" || ch.Name == "" {
		return nil
	}
	ev := c.event(connector.KindThread, ch.ID, ch.ID, snowflakeTime(ch.ID))
	ev.Payload.Title = ch.Name
	ev.Payload.URL = permalink(c.guild, ch.ID, ch.ID)
	ev.Payload.Author = &connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: ch.OwnerID}
	ev.Payload.Thread = ""
	return c.emit(ctx, sink, ev)
}
func (c *Connector) emitTombstone(ctx context.Context, sink connector.Sink, channelID, artifact, target string, at time.Time) error {
	ev := c.event(connector.KindTombstone, artifact, channelID, at)
	ev.Payload.Target = target
	ev.Payload.URL = permalink(c.guild, channelID, target)
	return c.emit(ctx, sink, ev)
}
func (c *Connector) emitReaction(ctx context.Context, sink connector.Sink, r reaction, remove bool) error {
	emoji := r.Emoji.Name
	if r.Emoji.ID != "" {
		emoji = r.Emoji.ID
	}
	if emoji == "" || r.UserID == "" {
		return nil
	}
	artifact := r.MessageID + ":reaction:" + r.UserID + ":" + url.QueryEscape(emoji)
	at := snowflakeTime(r.MessageID)
	if remove {
		ev := c.event(connector.KindTombstone, artifact+":tombstone", r.ChannelID, at)
		ev.Payload.Target = artifact
		ev.Payload.URL = permalink(c.guild, r.ChannelID, r.MessageID)
		return c.emit(ctx, sink, ev)
	}
	ev := c.event(connector.KindReaction, artifact, r.ChannelID, at)
	u := &user{ID: r.UserID}
	nick := ""
	if r.Member != nil {
		nick = r.Member.Nick
		if r.Member.User != nil {
			u = r.Member.User
		}
	}
	ev.Payload.Author = c.identity(u, nick)
	ev.Payload.Parent = r.MessageID
	ev.Payload.URL = permalink(c.guild, r.ChannelID, r.MessageID)
	native, _ := json.Marshal(map[string]string{"emoji": emoji})
	ev.Payload.Native = native
	return c.emit(ctx, sink, ev)
}
func permalink(guild, channel, message string) string {
	return "https://discord.com/channels/" + guild + "/" + channel + "/" + message
}

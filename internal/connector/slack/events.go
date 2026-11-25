package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

const permPublic = "perm:public"

type callback struct {
	Type      string          `json:"type"`
	TeamID    string          `json:"team_id"`
	Event     json.RawMessage `json:"event"`
	ExtShared bool            `json:"is_ext_shared_channel"`
}

type edited struct {
	TS string `json:"ts"`
}

type message struct {
	Type        string   `json:"type"`
	Subtype     string   `json:"subtype"`
	Channel     string   `json:"channel"`
	ChannelType string   `json:"channel_type"`
	User        string   `json:"user"`
	BotID       string   `json:"bot_id"`
	Text        string   `json:"text"`
	TS          string   `json:"ts"`
	ThreadTS    string   `json:"thread_ts"`
	ReplyCount  int      `json:"reply_count"`
	Edited      *edited  `json:"edited"`
	Message     *message `json:"message"`
	DeletedTS   string   `json:"deleted_ts"`
}

type reaction struct {
	Type     string `json:"type"`
	User     string `json:"user"`
	Reaction string `json:"reaction"`
	Item     struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
}

var content = map[string]bool{"": true, "bot_message": true, "thread_broadcast": true, "file_share": true, "me_message": true}

func (c *Connector) dispatch(ctx context.Context, sink connector.Sink, raw json.RawMessage) error {
	var cb callback
	if err := json.Unmarshal(raw, &cb); err != nil {
		return fmt.Errorf("decoding a slack event: %w", err)
	}
	if cb.Type != "event_callback" || cb.TeamID != c.team || cb.ExtShared {
		return nil
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(cb.Event, &head); err != nil {
		return fmt.Errorf("decoding a slack event: %w", err)
	}
	switch head.Type {
	case "message":
		var m message
		if err := json.Unmarshal(cb.Event, &m); err != nil {
			return fmt.Errorf("decoding a slack message event: %w", err)
		}
		if m.ChannelType != "channel" {
			return nil
		}
		switch m.Subtype {
		case "message_changed":
			if m.Message == nil {
				return nil
			}
			inner := *m.Message
			inner.Channel = m.Channel
			if inner.Subtype == "tombstone" {
				return c.emitTombstone(ctx, sink, m.Channel, inner.TS)
			}
			if !content[inner.Subtype] {
				return nil
			}
			return c.emitMessage(ctx, sink, inner)
		case "message_deleted":
			return c.emitTombstone(ctx, sink, m.Channel, m.DeletedTS)
		default:
			if !content[m.Subtype] {
				return nil
			}
			return c.emitMessage(ctx, sink, m)
		}
	case "channel_archive", "channel_unarchive", "channel_deleted", "channel_shared", "channel_unshared", "channel_convert_to_private":
		var change struct {
			Channel string `json:"channel"`
		}
		if err := json.Unmarshal(cb.Event, &change); err != nil {
			return err
		}
		if !c.configured(change.Channel) {
			return nil
		}
		private, err := c.Public(ctx, change.Channel)
		if err != nil {
			return err
		}
		return c.observeVisibility(ctx, sink, change.Channel, !private)
	case "reaction_added", "reaction_removed":
		var r reaction
		if err := json.Unmarshal(cb.Event, &r); err != nil {
			return fmt.Errorf("decoding a slack reaction event: %w", err)
		}
		if r.Item.Type != "message" {
			return nil
		}
		return c.emitReaction(ctx, sink, r, head.Type == "reaction_removed")
	}
	return nil
}

func tsTime(ts string) (time.Time, bool) {
	sec, micro, ok := strings.Cut(ts, ".")
	if !ok || len(micro) != 6 {
		return time.Time{}, false
	}
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || s <= 0 {
		return time.Time{}, false
	}
	u, err := strconv.ParseInt(micro, 10, 64)
	if err != nil || u < 0 {
		return time.Time{}, false
	}
	return time.Unix(s, u*1000).UTC(), true
}

func messageArtifact(channel, ts string) string { return channel + "/" + ts }

func permalink(channel, ts, thread string) string {
	link := "https://slack.com/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
	if thread != "" {
		link += "?thread_ts=" + thread + "&cid=" + channel
	}
	return link
}

var mention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

func (c *Connector) mentions(text string) []connector.Identity {
	var out []connector.Identity
	seen := map[string]bool{}
	for _, m := range mention.FindAllStringSubmatch(text, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: m[1]})
	}
	return out
}

func (c *Connector) author(m message) *connector.Identity {
	switch {
	case m.User != "" && m.BotID != "":
		return &connector.Identity{Source: c.source, Kind: connector.IdentityBot, NativeID: m.User}
	case m.User != "":
		return &connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: m.User}
	case m.BotID != "":
		return &connector.Identity{Source: c.source, Kind: connector.IdentityBot, NativeID: m.BotID}
	}
	return nil
}

func (c *Connector) event(kind connector.Kind, artifact, channel string, at time.Time) connector.Event {
	c.mu.Lock()
	restricted := c.restricted[channel]
	c.mu.Unlock()
	acl := connector.ACL{{Kind: connector.ACLPublic}}
	if restricted {
		acl = connector.ACL{{Kind: connector.ACLGroup, Source: c.source, NativeID: channel}}
	}
	return connector.Event{
		Source: c.source, NativeID: artifact, Kind: kind, Time: at,
		ACL:     acl,
		Payload: connector.Payload{Artifact: artifact, Container: connector.Container{Kind: connector.ContainerChannel, NativeID: channel}},
	}
}

func (c *Connector) emit(ctx context.Context, sink connector.Sink, ev connector.Event) error {
	if ev.ACL[0].Kind != connector.ACLPublic && ev.Payload.Revision == nil {
		ev.NativeID += "@perm:private"
		ev.Payload.Revision = &connector.Revision{Token: "perm:private"}
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
	at, ok := tsTime(m.TS)
	author := c.author(m)
	if !ok || author == nil || m.Channel == "" {
		return nil
	}
	artifact := messageArtifact(m.Channel, m.TS)
	ev := c.event(connector.KindMessage, artifact, m.Channel, at)
	ev.Payload.Text = m.Text
	ev.Payload.Author = author
	ev.Payload.Mentions = c.mentions(m.Text)
	ev.Payload.URL = permalink(m.Channel, m.TS, "")
	if m.ThreadTS != "" && m.ThreadTS != m.TS {
		if _, ok := tsTime(m.ThreadTS); ok {
			root := messageArtifact(m.Channel, m.ThreadTS)
			ev.Payload.Parent, ev.Payload.Thread = root, root
			ev.Payload.URL = permalink(m.Channel, m.TS, m.ThreadTS)
		}
	}
	var editedAt time.Time
	editedTS := ""
	if m.Edited != nil {
		if t, ok := tsTime(m.Edited.TS); ok && !t.Before(at) {
			editedAt, editedTS = t, m.Edited.TS
		}
	}
	sum, err := json.Marshal(struct {
		Author connector.Identity `json:"author"`
		Text   string             `json:"text"`
		Thread string             `json:"thread"`
		Edited string             `json:"edited"`
	}{*author, m.Text, ev.Payload.Thread, editedTS})
	if err != nil {
		return fmt.Errorf("hashing slack message %s: %w", artifact, err)
	}
	hash := sha256.Sum256(sum)
	permission := permPublic
	if ev.ACL[0].Kind != connector.ACLPublic {
		permission = "perm:private"
	}
	token := fmt.Sprintf("%x+%s", hash[:8], permission)
	ev.NativeID = artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: editedAt}
	return c.emit(ctx, sink, ev)
}

func (c *Connector) emitTombstone(ctx context.Context, sink connector.Sink, channel, ts string) error {
	at, ok := tsTime(ts)
	if !ok || channel == "" {
		return nil
	}
	target := messageArtifact(channel, ts)
	ev := c.event(connector.KindTombstone, target+":tombstone", channel, at)
	ev.Payload.Target = target
	ev.Payload.URL = permalink(channel, ts, "")
	return c.emit(ctx, sink, ev)
}

func (c *Connector) emitReaction(ctx context.Context, sink connector.Sink, r reaction, remove bool) error {
	at, ok := tsTime(r.Item.TS)
	if !ok || r.User == "" || r.Reaction == "" || r.Item.Channel == "" {
		return nil
	}
	message := messageArtifact(r.Item.Channel, r.Item.TS)
	artifact := message + ":reaction:" + r.User + ":" + url.QueryEscape(r.Reaction)
	if remove {
		ev := c.event(connector.KindTombstone, artifact+":tombstone", r.Item.Channel, at)
		ev.Payload.Target = artifact
		ev.Payload.URL = permalink(r.Item.Channel, r.Item.TS, "")
		return c.emit(ctx, sink, ev)
	}
	ev := c.event(connector.KindReaction, artifact, r.Item.Channel, at)
	ev.Payload.Author = &connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: r.User}
	ev.Payload.Parent = message
	ev.Payload.URL = permalink(r.Item.Channel, r.Item.TS, "")
	native, err := json.Marshal(map[string]string{"emoji": r.Reaction})
	if err != nil {
		return fmt.Errorf("encoding slack reaction %s: %w", artifact, err)
	}
	ev.Payload.Native = native
	return c.emit(ctx, sink, ev)
}

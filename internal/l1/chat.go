package l1

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// ChatWindow is a fixed 30 minute UTC source-time bucket. Channel messages
// (including parent-only reply chains) in the same channel and bucket form one
// document. A reply across a boundary belongs to its own bucket. Buckets do
// not use the first message as an anchor: late messages, edits and deletions
// change membership, never the key. An empty bucket has no document. The
// delimiter is unambiguous because the final component is a Unix timestamp.
const ChatWindow = 30 * time.Minute

// ChatWindowKey is the source-native id of a channel conversation.
func ChatWindowKey(ev connector.Event) string {
	start := ev.Time.UTC().Truncate(ChatWindow).Unix()
	return "chat:" + ev.Payload.Container.NativeID + ":" + strconv.FormatInt(start, 10)
}

// ParseChatWindowKey reads the container and half-open window from a key.
func ParseChatWindowKey(key string) (string, time.Time, bool) {
	rest, ok := strings.CutPrefix(key, "chat:")
	if !ok {
		return "", time.Time{}, false
	}
	cut := strings.LastIndex(rest, ":")
	if cut < 1 {
		return "", time.Time{}, false
	}
	seconds, err := strconv.ParseInt(rest[cut+1:], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	start := time.Unix(seconds, 0).UTC()
	return rest[:cut], start, start.Unix()%int64(ChatWindow.Seconds()) == 0
}

// ChatRoot reports a message that can head a conversation of its own: a
// channel message that is not itself in a thread. On a two-level chat source,
// such as Slack, a reply's `thread` is the message it answers
// (docs/connector-contract.md), and that message is the root of a chat_thread
// keyed by its own artifact as well as a member of its channel's window.
func ChatRoot(ev connector.Event) bool {
	return (ev.Kind == connector.KindMessage || ev.Payload.BaseKind == connector.KindMessage) &&
		ev.Payload.Container.Kind == connector.ContainerChannel && ev.Payload.Thread == ""
}

// BuildChatReplies builds the conversation a [ChatRoot] message heads from the
// current events that hang off it, and returns the replies it kept. A reply is
// an event whose `thread` names the message; one that names it only as its
// `parent` — a reaction, or a reply on a source whose replies stay in the
// channel's window — is not. A message with no reply heads no document, which
// is [ErrNotDistilled].
func BuildChatReplies(root connector.Event, events []connector.Event, resolver *principal.Resolver, repo config.Repo) (Document, []connector.Event, error) {
	if !ChatRoot(root) {
		return Document{}, nil, fmt.Errorf("%w: %s is not a channel message outside a thread", ErrNotDistilled, root.NativeID)
	}
	artifact := root.Payload.Artifact
	var replies []connector.Event
	answered := false
	for _, ev := range events {
		if ev.Payload.Thread != artifact {
			continue
		}
		replies = append(replies, ev)
		answered = answered || !control(ev)
	}
	if !answered {
		return Document{}, nil, fmt.Errorf("%w: nothing answers %s", ErrNotDistilled, artifact)
	}
	doc, err := build(Input{Root: root, Children: replies, Resolver: resolver, Repo: repo}, KindChatThread, artifact, false)
	if err != nil {
		return Document{}, nil, err
	}
	return doc, replies, nil
}

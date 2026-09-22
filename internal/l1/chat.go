package l1

import (
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
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

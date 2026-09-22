package l1

import (
	"fmt"
	"slices"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// BurstMinThreadMessages, BurstMinRunMessages and BurstMaxAuthorPercent are
// intentionally fixed gates. A thread needs eight current messages; a run
// needs three; its author may write at most 40 percent of the thread's current
// messages. Equality passes. The native thread root is not a message.
const (
	BurstMinThreadMessages = 8
	BurstMinRunMessages    = 3
	BurstMaxAuthorPercent  = 40
)

// BurstPrefix is the namespace of bursts belonging to a conversation.
func BurstPrefix(threadKey string) string { return "burst:" + threadKey + ":" }

// BuildChatBursts returns disjoint, source-time-ordered single-author runs.
// A run is anchored by its first message artifact so edits keep its ID. A
// deleted anchor changes the ID; the distiller removes the former row.
func BuildChatBursts(threadKey string, messages []connector.Event, resolver *principal.Resolver, repo config.Repo) ([]Document, error) {
	if len(messages) < BurstMinThreadMessages {
		return nil, nil
	}
	ordered := slices.Clone(messages)
	slices.SortFunc(ordered, byConversationOrder)
	counts := map[string]int{}
	for _, ev := range ordered {
		if ev.Payload.Author != nil {
			counts[authorKey(ev.Payload.Author)]++
		}
	}
	var out []Document
	for start := 0; start < len(ordered); {
		author := ordered[start].Payload.Author
		end := start + 1
		if author != nil {
			for end < len(ordered) && ordered[end].Payload.Author != nil && authorKey(ordered[end].Payload.Author) == authorKey(author) {
				end++
			}
		}
		if author != nil && end-start >= BurstMinRunMessages && counts[authorKey(author)]*100 <= BurstMaxAuthorPercent*len(ordered) {
			key := BurstPrefix(threadKey) + ordered[start].Payload.Artifact
			doc, err := build(Input{Root: ordered[start], Children: ordered[start+1 : end], Resolver: resolver, Repo: repo}, KindChatBurst, key, true)
			if err != nil {
				return nil, fmt.Errorf("building burst %s: %w", key, err)
			}
			// The ACL fold may omit a message with no common grant.
			// A burst must quote its entire contiguous run or not exist.
			if len(doc.L0Refs) == end-start {
				out = append(out, doc)
			}
		}
		start = end
	}
	return out, nil
}

func authorKey(author *connector.Identity) string {
	return author.Source + ":" + string(author.Kind) + ":" + author.NativeID
}

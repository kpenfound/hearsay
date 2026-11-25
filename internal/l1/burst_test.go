package l1_test

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func burstMessages(authors string) []connector.Event {
	var out []connector.Event
	for i, author := range authors {
		id := "m" + strconv.Itoa(i)
		ev := event(connector.KindMessage, id, at(0).Add(time.Duration(i)*time.Minute), who(string(author), string(author)), "", "tangent "+id)
		ev.Payload.Container = connector.Container{Kind: connector.ContainerChannel, NativeID: "C1"}
		ev.Payload.URL = "https://discord.com/channels/g/C1/" + id
		out = append(out, ev)
	}
	return out
}

func TestChatBurstGates(t *testing.T) {
	for _, tt := range []struct {
		name, authors string
		want          int
	}{
		{"qualifying tangent", "AAABCBDE", 1},
		{"short thread", "AAABCBD", 0},
		{"short run", "AABCBDED", 0},
		{"common author", "AAAABBCD", 0},
		{"disjoint runs", "AAABBBCDEF", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := l1.BuildChatBursts("th1", burstMessages(tt.authors), nil, testRepo)
			if err != nil || len(docs) != tt.want {
				t.Fatalf("BuildChatBursts = %d, %v; want %d", len(docs), err, tt.want)
			}
		})
	}
}

func TestChatBurstQuotesItsRunAndNamesItsSource(t *testing.T) {
	docs, err := l1.BuildChatBursts("th1", burstMessages("AAABCBDE"), nil, testRepo)
	if err != nil || len(docs) != 1 {
		t.Fatalf("BuildChatBursts = %d, %v; want 1", len(docs), err)
	}
	if doc := docs[0]; doc.Kind != l1.KindChatBurst || len(doc.L0Refs) != 3 || doc.Source.URL == "" || doc.Source.NativeID == "" {
		t.Errorf("burst = %+v", doc)
	}
}

func TestChatBurstsOfOneThreadDoNotOverlap(t *testing.T) {
	docs, err := l1.BuildChatBursts("th1", burstMessages("AAABBBCDEF"), nil, testRepo)
	if err != nil || len(docs) != 2 {
		t.Fatalf("BuildChatBursts = %d, %v; want 2", len(docs), err)
	}
	for _, ref := range docs[0].L0Refs {
		if slices.Contains(docs[1].L0Refs, ref) {
			t.Errorf("bursts overlap on %s", ref)
		}
	}
	if docs[0].Source.NativeID == docs[1].Source.NativeID {
		t.Errorf("bursts share the source id %s", docs[0].Source.NativeID)
	}
}

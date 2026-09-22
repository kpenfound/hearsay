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

func TestChatBurstGatesAndProvenance(t *testing.T) {
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
			messages := burstMessages(tt.authors)
			docs, err := l1.BuildChatBursts("th1", messages, nil, testRepo)
			if err != nil || len(docs) != tt.want {
				t.Fatalf("BuildChatBursts = %d, %v; want %d", len(docs), err, tt.want)
			}
			for _, doc := range docs {
				if doc.Kind != l1.KindChatBurst || len(doc.L0Refs) != 3 || doc.Source.URL == "" || doc.Source.NativeID == "" {
					t.Errorf("burst = %+v", doc)
				}
				if slices.Contains(doc.L0Refs, connector.EventID(source, "m3")) && tt.name == "disjoint runs" {
					// m3 belongs only to the second run.
					if doc.Source.NativeID == l1.BurstPrefix("th1")+"m0" {
						t.Error("runs overlap")
					}
				}
			}
		})
	}
}

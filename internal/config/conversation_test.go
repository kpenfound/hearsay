package config_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
)

// The conversation source's example in docs/config.md is a whole configuration:
// the source, its owner and the agent allowed to post, each with the identity
// that resolves their events, and a scope that holds it. It loads the way a
// service loads it, and the source reads back as the one the example names.
func TestDocsConversationSourceLoads(t *testing.T) {
	root := writeFiles(t, filesUnder(t, docExamples(t), "conversation"))
	repo, err := config.Load(filepath.Join(root, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(the conversation source example) = %v, want no error", err)
	}
	if len(repo.Sources) != 1 || repo.Sources[0].Type != conversation.Type {
		t.Fatalf("sources = %+v, want the one conversation source", repo.Sources)
	}
	owner, agents, ok := conversation.OwnerOf(repo.Sources[0])
	if !ok || owner != "kyle" || !slices.Equal(agents, []string{"kern"}) {
		t.Errorf("OwnerOf = %q, %v, %v; want kyle owning it and kern allowed to post", owner, agents, ok)
	}
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
)

// conversationExample is the conversation source's example in docs/config.md,
// the whole configuration it documents.
func conversationExample(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "config.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)<!-- example: conversation/hearsay.yaml -->\n```yaml\n(.*?)```").FindSubmatch(raw)
	if m == nil {
		t.Fatal("docs/config.md has no conversation source example")
	}
	return string(m[1])
}

// The documented conversation source is accepted by `hearsay config validate`,
// and the connector the binary registers for it builds from it: what a person
// copies out of the docs starts a service, with the agent's token in its
// environment.
func TestDocumentedConversationSourceValidatesAndBuilds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hearsay.yaml"), []byte(conversationExample(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"config", "validate", dir}, &out, &errOut); err != nil {
		t.Fatalf("hearsay config validate = %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "is valid") || !strings.Contains(out.String(), "kern") {
		t.Errorf("hearsay config validate printed %q, want the configuration reported valid with its source", out.String())
	}

	repo, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEARSAY_KERN_TOKEN", "kern-token")
	src := repo.Sources[0]
	c, err := connectorRegistry(repo.Principals, repo.Sources).New(t.Context(), src)
	if err != nil {
		t.Fatalf("building the documented conversation source = %v", err)
	}
	if _, ok := c.(connector.Pusher); !ok {
		t.Errorf("the conversation source is not a pusher, so nothing mounts it at /hooks/%s", src.ID)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/config/configtest"
	"github.com/kpenfound/hearsay/internal/connector"
)

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

func TestDocumentedConversationSourceValidatesAndBuilds(t *testing.T) {
	dir := configtest.Write(t, map[string]string{"hearsay.yaml": conversationExample(t)})
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

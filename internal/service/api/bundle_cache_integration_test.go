//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

func cacheTestDB(t *testing.T) string {
	t.Helper()
	adminURL := os.Getenv("HEARSAY_DATABASE_URL")
	if adminURL == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	admin, err := db.Open(t.Context(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("bundle_cache_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropper, err := db.Open(context.Background(), adminURL)
		if err != nil {
			t.Errorf("connecting to drop cache test database: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping cache test database: %v", err)
		}
	})
	base, query, found := strings.Cut(adminURL, "?")
	url := base[:strings.LastIndex(base, "/")+1] + name
	if found {
		url += "?" + query
	}
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	return url
}

func TestBundleCacheFollowsCallInputsAndLayerWrites(t *testing.T) {
	url := cacheTestDB(t)
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Setenv("CACHE_TEST_KYLE", "kyle-token")
	t.Setenv("CACHE_TEST_SAM", "sam-token")
	t.Setenv("CACHE_TEST_SHED", "shed-token")
	repo := config.Repo{Principals: []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "CACHE_TEST_KYLE"},
		{ID: "sam", Kind: principal.KindHuman, TokenEnv: "CACHE_TEST_SAM"},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "CACHE_TEST_SHED"},
	}}
	calls, err := NewCalls(pool, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := "code:cache-test"
	kyle := Caller{Principal: "kyle"}
	request := func(c *Calls, caller Caller, scope, directive string) []byte {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"scope": scope, "directive": directive})
		body, err := c.Call(t.Context(), caller, "get_bundle", args)
		if err != nil {
			t.Fatalf("get_bundle(%s, %s, %s): %v", caller.Principal, scope, directive, err)
		}
		return body
	}
	entries := func(want int) {
		t.Helper()
		if got := calls.cache.order.Len(); got != want {
			t.Fatalf("cache entries = %d, want %d", got, want)
		}
	}
	var revision int64
	readRevision := func() int64 {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM bundle_changes`).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		return revision
	}

	first := request(calls, kyle, scope, "")
	entries(1)
	beforeAudit := readRevision()
	assembler := calls.assembler
	calls.assembler = nil // A second assembly would panic: this call must use the cache.
	second := request(calls, kyle, scope, "")
	calls.assembler = assembler
	entries(1)
	if !bytes.Equal(first, second) || readRevision() != beforeAudit {
		t.Fatal("cache hit changed bytes or advanced the non-audit watermark")
	}
	fresh, err := NewCalls(pool, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, request(fresh, kyle, scope, "")) {
		t.Fatal("cached bytes differ from a fresh assembly")
	}
	var audits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events WHERE source = $1 AND kind = 'audit'`, AuditSource).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audit events = %d, %v, want 3", audits, err)
	}
	request(calls, Caller{Principal: "sam"}, scope, "")
	request(calls, Caller{Principal: "kyle", Agent: "shed"}, scope, "")
	request(calls, kyle, scope, "evt:unknown:directive")
	request(calls, kyle, scope+":other", "")
	entries(5)

	event := connector.Event{Source: "cache-test", NativeID: "issue", Kind: connector.KindIssue, Time: time.Now().UTC(),
		Payload: connector.Payload{Artifact: "issue", Title: "cache invalidation", Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "cache-test"}, Author: &connector.Identity{Source: "cache-test", Kind: connector.IdentityUser, NativeID: "author"}},
		ACL:     connector.ACL{{Kind: connector.ACLPublic}}}
	if _, err := l0.New(pool).Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	preDistill := request(calls, kyle, scope, "")
	entries(6)
	baseRevision := readRevision()
	doc := l1.Document{ID: l1.DocID("cache-test", "issue"), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: "cache-test", NativeID: "issue"}, L0Refs: []string{connector.EventID("cache-test", "issue")},
		Time: l1.Times{Created: event.Time, Updated: event.Time, LastActivity: event.Time}, Scope: []string{scope},
		ACL: event.ACL, Text: "first summary", RawText: "first summary", Body: l1.Body{Summary: "first summary", OutcomeKind: l1.OutcomeNone}}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	if readRevision() <= baseRevision {
		t.Fatal("L1 write did not advance watermark")
	}
	withDoc := request(calls, kyle, scope, "")
	entries(7)
	if bytes.Equal(preDistill, withDoc) {
		t.Fatal("L1 distillation did not change the bundle")
	}
	topic := l2.Topic{ID: l2.TopicID("cache-test", "issue", 0, "cache stance"), Scope: "cache-test", Name: "cache stance",
		About: []string{scope}, ACL: event.ACL, OpenedBy: doc.ID}
	graph := l2.New(pool)
	if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
		t.Fatal(err)
	}
	request(calls, kyle, scope, "")
	entries(8)
	stance := l2.Stance{ID: l2.StanceID(topic.ID, doc.ID, "cache position", event.Time, l2.TierInferred),
		TopicID: topic.ID, Position: "cache position", Author: "kyle", StatedAt: event.Time,
		Evidence: []string{doc.ID}, Tier: l2.TierInferred, Judgement: l2.JudgementUnknown, ACL: event.ACL}
	if _, _, err := graph.AppendStance(t.Context(), stance, event.Time); err != nil {
		t.Fatal(err)
	}
	withStance := request(calls, kyle, scope, "")
	entries(9)
	if bytes.Equal(withDoc, withStance) {
		t.Fatal("L2 stance append did not change the bundle")
	}
	doc.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: "cache-test", NativeID: "someone-else"}}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	resynced := request(calls, kyle, scope, "")
	entries(10)
	if bytes.Equal(withStance, resynced) {
		t.Fatal("ACL re-sync did not change the bundle")
	}
	if err := l2.New(pool).PutEntity(t.Context(), l2.Entity{ID: scope, Type: l2.TypeProject, Name: "cache test", Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	request(calls, kyle, scope, "")
	entries(11)
	for i := 0; i < bundleCacheLimit+3; i++ {
		request(calls, kyle, fmt.Sprintf("code:cache-test:%d", i), "")
		if got := calls.cache.order.Len(); got > bundleCacheLimit {
			t.Fatalf("cache grew to %d entries, bound is %d", got, bundleCacheLimit)
		}
	}
	entries(bundleCacheLimit)
	if _, ok := calls.cache.items[bundleKey{scope: scope, principal: "kyle", revision: baseRevision}]; ok {
		t.Fatal("oldest entry was not evicted")
	}
}

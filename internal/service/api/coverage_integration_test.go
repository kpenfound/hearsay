//go:build integration

package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/api"
)

type coverageMessage struct {
	Exchange string    `json:"exchange"`
	Role     string    `json:"role"`
	At       time.Time `json:"at"`
	Text     string    `json:"text"`
}

type coverageRecording struct {
	Conversation string            `json:"conversation"`
	Messages     []coverageMessage `json:"messages"`
	States       map[string]struct {
		Edit     map[string]string `json:"edit"`
		Add      []coverageMessage `json:"add"`
		Segments []l1.MeetingSpan  `json:"segments"`
	} `json:"states"`
}

func loadCoverageRecording(t *testing.T) coverageRecording {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "conversations", "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec coverageRecording
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

type conversationWorld struct {
	*world
	rec    coverageRecording
	thread string

	texts map[string]coverageMessage
}

func newConversationWorld(t *testing.T) *conversationWorld {
	t.Helper()
	w := newWorld(t)
	rec := loadCoverageRecording(t)
	return &conversationWorld{world: w, rec: rec, thread: rec.Conversation + "-" + w.src, texts: map[string]coverageMessage{}}
}

func (c *conversationWorld) event(m coverageMessage) connector.Event {
	author := connector.Identity{Source: c.src, Kind: connector.IdentityUser, NativeID: kyleNode}
	if m.Role == conversation.RoleAssistant {
		author = connector.Identity{Source: c.src, Kind: connector.IdentityAgent, NativeID: "shed"}
	}
	native, _ := json.Marshal(conversation.Native{Conversation: c.thread, Exchange: m.Exchange, Role: m.Role})
	artifact := conversation.Artifact(c.thread, m.Exchange, m.Role)
	sum := sha256.Sum256([]byte(m.Text))
	token := hex.EncodeToString(sum[:8])
	return connector.Event{
		Source: c.src, NativeID: artifact + "@" + token, Kind: conversation.Kind, Time: m.At,
		Payload: connector.Payload{
			Artifact: artifact, BaseKind: connector.KindMessage, Thread: c.thread, Text: m.Text, Author: &author,
			Container: connector.Container{Kind: conversation.ContainerStream, NativeID: kyleNode},
			Revision:  &connector.Revision{Token: token}, Native: native,
		},
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: c.src, NativeID: kyleNode}},
	}
}

func (c *conversationWorld) post(t *testing.T, m coverageMessage) string {
	t.Helper()
	c.texts[m.Exchange+":"+m.Role] = m
	ev := c.event(m)
	if _, err := l0.New(c.pool).Append(t.Context(), ev); err != nil {
		t.Fatalf("Append(%s) = %v", ev.NativeID, err)
	}
	return connector.EventID(ev.Source, ev.NativeID)
}

func (c *conversationWorld) id(exchange, role string) string {
	ev := c.event(c.texts[exchange+":"+role])
	return connector.EventID(ev.Source, ev.NativeID)
}

func (c *conversationWorld) postAll(t *testing.T, messages []coverageMessage) {
	t.Helper()
	for _, m := range messages {
		c.post(t, m)
	}
}

func (c *conversationWorld) publish(t *testing.T, spans []l1.MeetingSpan, read ...string) []l1.Document {
	t.Helper()
	ctx := t.Context()
	current, err := l0.New(c.pool).Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: c.src, Thread: c.thread}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	lines := l1.ConversationLines(current)
	if len(read) == 0 {
		for _, line := range lines {
			read = append(read, line.Event.ID)
		}
	}
	docs, err := l1.BuildConversationSegments(c.src, c.thread, lines, spans, nil, config.Repo{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(docs))
	err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		store := l1.New(tx)
		for i := range docs {
			docs[i].Scope = []string{c.scope}
			docs[i].Body = l1.Body{Summary: "A topic of the conversation.", OutcomeKind: l1.OutcomeNone}
			docs[i].Text = docs[i].Body.Summary
			ids[i] = docs[i].ID
			if _, err := store.Put(ctx, docs[i]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3 AND NOT (id = ANY($4))`,
			c.src, l1.KindConversationSegment, l1.ConversationSegmentPrefix(c.thread), ids); err != nil {
			return err
		}
		_, err := store.PutConversationDistillation(ctx, l1.ConversationDistillation{Source: c.src, Thread: c.thread, Inputs: read, Documents: ids})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

type coverageAnswer struct {
	Results []api.CoverageResult `json:"results"`
}

func (w *world) ask(t *testing.T, caller api.Caller, ids ...string) []api.CoverageResult {
	t.Helper()
	var got coverageAnswer
	if err := json.Unmarshal(w.http(t, caller, "coverage", map[string]any{"event_ids": ids}), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != len(ids) {
		t.Fatalf("%d results for %d ids: %+v", len(got.Results), len(ids), got.Results)
	}
	for i, r := range got.Results {
		if r.EventID != ids[i] {
			t.Fatalf("result %d is for %q, asked %q", i, r.EventID, ids[i])
		}
	}
	return got.Results
}

func expect(t *testing.T, got api.CoverageResult, state string, documents ...string) {
	t.Helper()
	slices.Sort(documents)
	if got.State != state || len(got.Documents) != len(documents) || len(documents) > 0 && !slices.Equal(got.Documents, documents) {
		t.Errorf("%s = %s %v, want %s %v", got.EventID, got.State, got.Documents, state, documents)
	}
}

func (c *conversationWorld) base() []l1.MeetingSpan { return c.rec.States["base"].Segments }

func TestCoverageGoesFromPendingToCoveredAndLeavesTheGreetingOut(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	greeting, decision, deploy := c.id("x-1", "owner"), c.id("x-3", "owner"), c.id("x-4", "assistant")

	for _, r := range c.ask(t, kyle, greeting, decision, deploy) {
		expect(t, r, "pending")
	}

	for _, r := range c.ask(t, shedKyle, greeting, decision) {
		expect(t, r, "pending")
	}

	docs := c.publish(t, c.base())
	byTopic := map[string]string{}
	for _, d := range docs {
		switch {
		case slices.Contains(d.L0Refs, decision):
			byTopic["cache"] = d.ID
		case slices.Contains(d.L0Refs, deploy):
			byTopic["deploy"] = d.ID
		}
	}
	if len(docs) != 2 || byTopic["cache"] == "" || byTopic["deploy"] == "" {
		t.Fatalf("documents = %+v", docs)
	}
	got := c.ask(t, kyle, greeting, decision, deploy, c.id("x-1", "assistant"), c.id("x-2", "assistant"))
	expect(t, got[0], "excluded")
	expect(t, got[1], "covered", byTopic["cache"])
	expect(t, got[2], "covered", byTopic["deploy"])
	expect(t, got[3], "excluded")
	expect(t, got[4], "covered", byTopic["cache"])
}

func TestCoverageExcludesEveryMessageWhenSegmentationSelectsNothing(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	var ids []string
	for _, m := range c.rec.Messages {
		ids = append(ids, c.id(m.Exchange, m.Role))
	}
	for _, r := range c.ask(t, kyle, ids...) {
		expect(t, r, "pending")
	}
	if docs := c.publish(t, c.rec.States["nothing"].Segments); len(docs) != 0 {
		t.Fatalf("documents = %+v", docs)
	}
	for _, r := range c.ask(t, kyle, ids...) {
		expect(t, r, "excluded")
	}
}

func TestCoverageStaysPendingWithNoPublishedDistillationAndFollowsTheNextOne(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	decision := c.id("x-3", "owner")

	for range 2 {
		expect(t, c.ask(t, kyle, decision)[0], "pending")
	}
	docs := c.publish(t, c.base())
	var cited string
	for _, d := range docs {
		if slices.Contains(d.L0Refs, decision) {
			cited = d.ID
		}
	}
	expect(t, c.ask(t, kyle, decision)[0], "covered", cited)
}

func TestCoverageIsPendingForARewriteUntilADistillationReadsIt(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	older := c.id("x-3", "owner")
	published := c.publish(t, c.base())
	expect(t, c.ask(t, kyle, older)[0], "covered", published[0].ID)

	rewrite := c.rec.States["rewrite"]
	var edited coverageMessage
	for _, m := range c.rec.Messages {
		if m.Exchange+":"+m.Role == "x-3:owner" {
			edited = m
			edited.Text = rewrite.Edit["x-3:owner"]
		}
	}
	newer := c.post(t, edited)
	if newer == older {
		t.Fatal("the rewrite did not make a new revision")
	}

	if held, err := l1.New(c.pool).Holders(t.Context(), []string{older, newer}); err != nil || len(held[older]) == 0 || len(held[newer]) != 0 {
		t.Fatalf("holders = %+v, %v", held, err)
	}
	for name, id := range map[string]string{"the new revision": newer, "the superseded id": older} {
		t.Run(name, func(t *testing.T) {
			expect(t, c.ask(t, kyle, id)[0], "pending")
		})
	}

	deploy := c.id("x-4", "owner")
	if got := c.ask(t, kyle, deploy)[0]; got.State != "covered" {
		t.Errorf("an untouched message = %+v, want covered", got)
	}

	published = c.publish(t, rewrite.Segments)
	var now []string
	for _, d := range published {
		if slices.Contains(d.L0Refs, newer) {
			now = append(now, d.ID)
		}
	}
	if len(now) != 1 {
		t.Fatalf("documents quoting the new revision = %v", now)
	}
	for _, id := range []string{newer, older} {
		expect(t, c.ask(t, kyle, id)[0], "covered", now...)
	}
}

func TestCoverageIsPendingForAMessageThatArrivedWhileAnAttemptRan(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	var read []string
	for _, m := range c.rec.Messages {
		read = append(read, c.id(m.Exchange, m.Role))
	}
	late := c.rec.States["late"]
	arrived := c.post(t, late.Add[0])

	current := c.publish(t, c.base(), read...)
	if len(current) == 0 {
		t.Fatal("no documents")
	}
	got := c.ask(t, kyle, arrived, read[len(read)-1])
	expect(t, got[0], "pending")
	if got[1].State != "covered" && got[1].State != "excluded" {
		t.Errorf("a message the attempt read = %+v", got[1])
	}

	docs := c.publish(t, late.Segments)
	var cited string
	for _, d := range docs {
		if slices.Contains(d.L0Refs, arrived) {
			cited = d.ID
		}
	}
	expect(t, c.ask(t, kyle, arrived)[0], "covered", cited)
}

func TestCoverageExcludesADeletedMessage(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	c.publish(t, c.base())
	decision, deploy := c.id("x-3", "owner"), c.id("x-4", "owner")
	if got := c.ask(t, kyle, decision, deploy); got[0].State != "covered" || got[1].State != "covered" {
		t.Fatalf("before = %+v", got)
	}
	cfg := repo(c.src)
	if _, err := deletion.Apply(t.Context(), c.pool, cfg, deletion.Selector{ArtifactSource: c.src, ArtifactID: conversation.Artifact(c.thread, "x-3", "owner")}, "forget that", "kyle"); err != nil {
		t.Fatal(err)
	}
	got := c.ask(t, kyle, decision, deploy)
	expect(t, got[0], "excluded")
	if got[1].State != "covered" {
		t.Errorf("a message not deleted = %+v, want covered", got[1])
	}
}

func TestCoverageAnswersAnUnreadableIdAsANonexistentOne(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	c.publish(t, c.base())
	decision := c.id("x-3", "owner")

	readable := connector.EventID(c.src, c.project+"#12")
	nobody := connector.EventID(c.src, "c-nobody/x-0:owner@0000000000000000")

	mask := func(body []byte, id string) string { return strings.ReplaceAll(string(body), id, "<id>") }
	for who, caller := range map[string]api.Caller{"sam": sam, "an agent acting for sam": {Principal: "sam", Agent: "shed"}} {
		unreadable := c.http(t, caller, "coverage", map[string]any{"event_ids": []string{decision}})
		missing := c.http(t, caller, "coverage", map[string]any{"event_ids": []string{nobody}})
		if mask(unreadable, decision) != mask(missing, nobody) {
			t.Errorf("%s: unreadable %s, nonexistent %s", who, unreadable, missing)
		}
		if bytes.Contains(unreadable, []byte("covered")) || bytes.Contains(unreadable, []byte("documents")) {
			t.Errorf("%s: %s", who, unreadable)
		}

		both := c.ask(t, caller, decision, nobody, readable)
		for _, r := range both {
			expect(t, r, "excluded")
		}
	}

	expect(t, c.ask(t, kyle, decision)[0], "covered", c.publishedDocument(t, decision))
	expect(t, c.ask(t, kyle, nobody)[0], "excluded")
}

func (c *conversationWorld) publishedDocument(t *testing.T, event string) string {
	t.Helper()
	held, err := l1.New(c.pool).Holders(t.Context(), []string{event})
	if err != nil || len(held[event]) != 1 {
		t.Fatalf("holders of %s = %+v, %v", event, held, err)
	}
	return held[event][0].ID
}

func TestCoverageOverHTTPAndMCPAndItsRefusals(t *testing.T) {
	c := newConversationWorld(t)
	c.postAll(t, c.rec.Messages)
	c.publish(t, c.base())
	var embeds atomic.Int64
	calls, err := api.NewCalls(c.pool, repo(c.src), countingEmbedder{&embeds})
	if err != nil {
		t.Fatal(err)
	}
	w := *c.world
	w.calls = calls
	w.server = httptest.NewServer(api.Handler(calls, c.pool))
	t.Cleanup(w.server.Close)

	ids := []string{c.id("x-1", "owner"), c.id("x-3", "owner"), "evt:nobody:none"}
	args := map[string]any{"event_ids": ids}
	overHTTP := w.http(t, shedKyle, "coverage", args)
	overMCP, isError := w.mcp(t, shedKyle, "coverage", args)
	if isError || !bytes.Equal(overHTTP, overMCP) {
		t.Errorf("HTTP %s\nMCP  %s (error %v)", overHTTP, overMCP, isError)
	}
	var got coverageAnswer
	if err := json.Unmarshal(overHTTP, &got); err != nil || len(got.Results) != 3 {
		t.Fatalf("%s, %v", overHTTP, err)
	}
	expect(t, got.Results[0], "excluded")
	expect(t, got.Results[1], "covered", c.publishedDocument(t, ids[1]))
	expect(t, got.Results[2], "excluded")
	if !bytes.Contains(overHTTP, []byte(`"documents":["`+c.publishedDocument(t, ids[1])+`"]`)) || bytes.Count(overHTTP, []byte(`"documents"`)) != 1 {
		t.Errorf("documents appear only on a covered result: %s", overHTTP)
	}

	many := make([]string, api.MaxCoverageEvents+1)
	for i := range many {
		many[i] = "evt:x:" + strconv.Itoa(i)
	}
	for name, body := range map[string]any{
		"an empty list":  map[string]any{"event_ids": []string{}},
		"no ids":         map[string]any{},
		"too many ids":   map[string]any{"event_ids": many},
		"an empty id":    map[string]any{"event_ids": []string{""}},
		"an unknown key": map[string]any{"event_ids": ids, "scope": "x"},
		"a wrong type":   map[string]any{"event_ids": "evt:x"},
		"a number as id": map[string]any{"event_ids": []int{1}},
	} {
		raw, _ := json.Marshal(body)
		status, answer := w.post(t, "/v1/coverage", kyle, string(raw))
		var refusal struct{ Error, Code string }
		if err := json.Unmarshal(answer, &refusal); status != http.StatusBadRequest || err != nil || refusal.Code != "invalid_request" || refusal.Error == "" {
			t.Errorf("%s = %d %s, want 400 invalid_request", name, status, answer)
		}
	}
	if status, _ := w.post(t, "/v1/coverage", kyle, `not json`); status != http.StatusBadRequest {
		t.Errorf("not json = %d", status)
	}
	exactly := make([]string, api.MaxCoverageEvents)
	copy(exactly, many)
	if r := w.ask(t, kyle, exactly...); len(r) != api.MaxCoverageEvents {
		t.Errorf("%d ids answered %d", len(exactly), len(r))
	}
	if embeds.Load() != 0 {
		t.Errorf("coverage embedded %d times; it makes no model call", embeds.Load())
	}
}

type countingEmbedder struct{ n *atomic.Int64 }

func (e countingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.n.Add(1)
	return nil, nil
}

func (countingEmbedder) Dimensions() int { return 3 }

//go:build integration

package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func TestAStanceRestingOnAConversationIsOnlyTheOwners(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	graph := l2.New(w.pool)
	traverseEntity(t, w, w.scope)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	ownerOnly := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}

	openedBy := w.putIn(w.scope, w.project+"#50", l1.KindIssue, 20, "The cache is shared by every service.", public)
	topic := l2.Topic{ID: l2.TopicID(w.src, openedBy, 0, "who owns the cache"), Scope: w.src, Name: "who owns the cache",
		About: []string{w.scope}, ACL: public, OpenedBy: openedBy}
	if _, err := graph.OpenTopic(ctx, topic); err != nil {
		t.Fatal(err)
	}

	conversation := conversationDoc(t, w, "c-cache", "x-1", 21, ownerOnly)
	alone := l2.Topic{ID: l2.TopicID(w.src, conversation, 0, "the owner's cache decision"), Scope: w.src, Name: "the owner's cache decision",
		About: []string{w.scope}, ACL: ownerOnly, OpenedBy: conversation}
	if _, err := graph.OpenTopic(ctx, alone); err != nil {
		t.Fatal(err)
	}
	stance := func(tp l2.Topic, doc, position string, hour int, judgement l2.Judgement, acl connector.ACL) l2.Stance {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		st, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position, Author: "kyle",
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, Judgement: judgement, ACL: acl,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	team := stance(topic, openedBy, "each service owns its cache lines", 20, l2.JudgementUnknown, public)
	const decision = "KERN-CONVERSATION-DECISION the platform team owns the cache"
	mixed := stance(topic, conversation, decision, 22, l2.JudgementChanges, ownerOnly)
	sole := stance(alone, conversation, decision+" (alone)", 23, l2.JudgementUnknown, ownerOnly)

	later := w.putIn(w.scope, w.project+"#51", l1.KindIssue, 24, "Each service owns its cache again.", public)
	after := stance(topic, later, "each service owns its cache again", 24, l2.JudgementChanges, public)

	strict := repo(w.src)
	strict.Authority = loadAuthority(t, "scope: \"*\"\nranking: [dm, merged_pr]\n")
	calls, err := api.NewCalls(w.pool, strict, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, authority := range map[string]*api.Calls{"default ranking": w.calls, "the conversation ranks first": calls} {
		t.Run(name, func(t *testing.T) {
			w := *w
			w.calls = authority
			w.server = httptest.NewServer(api.Handler(authority, w.pool))
			t.Cleanup(w.server.Close)

			secrets := []string{decision, mixed.ID, sole.ID, conversation, alone.ID, "the owner's cache decision"}
			read := func(caller api.Caller) map[string]string {
				return map[string]string{
					"get_bundle":     string(w.http(t, caller, "get_bundle", map[string]any{"scope": w.scope})),
					"stance_history": string(w.http(t, caller, "stance_history", map[string]any{"topic": topic.ID})),
					"traverse": func() string {
						_, body := walk(t, &w, caller, traverseArgs("entity", w.scope, map[string]any{"edge": "topics"}, map[string]any{"edge": "stances"}))
						return body
					}(),
					"traverse from the team's stance": func() string {
						_, body := walk(t, &w, caller, traverseArgs("stance", team.ID, map[string]any{"edge": "superseded_by", "depth": "1..4"}))
						return body
					}(),
					"traverse from the later stance": func() string {
						_, body := walk(t, &w, caller, traverseArgs("stance", after.ID, map[string]any{"edge": "supersedes", "depth": "1..4"}))
						return body
					}(),
				}
			}
			for who, caller := range map[string]api.Caller{
				"sam":                     sam,
				"an agent acting for sam": {Principal: "sam", Agent: "shed"},
			} {
				for call, body := range read(caller) {
					for _, secret := range secrets {
						if strings.Contains(body, secret) {
							t.Errorf("%s from %s names %q: %s", call, who, secret, body)
						}
					}
				}

				if status, got := w.post(t, "/v1/stance_history", caller, `{"topic":"`+alone.ID+`"}`); status != 404 {
					t.Errorf("stance_history of the conversation's own topic for %s = %d %s, want 404", who, status, got)
				}
				var theirs api.History
				if err := json.Unmarshal([]byte(read(caller)["stance_history"]), &theirs); err != nil {
					t.Fatal(err)
				}
				for _, st := range theirs.Stances {
					if st.ID == mixed.ID || st.Position == decision || st.Supersedes == mixed.ID {
						t.Errorf("%s's history holds the owner's stance: %+v", who, st)
					}
				}
			}

			owner := read(kyle)
			for call, wants := range map[string][]string{
				"stance_history": {mixed.ID, decision},
				"traverse":       {mixed.ID, alone.ID},
			} {
				for _, want := range wants {
					if !strings.Contains(owner[call], want) {
						t.Errorf("%s for the owner lacks %q: %s", call, want, owner[call])
					}
				}
			}
			if strings.Contains(name, "ranks first") {
				if !strings.Contains(owner["get_bundle"], decision) {
					t.Errorf("get_bundle for the owner lacks the decision it is current on: %s", owner["get_bundle"])
				}
				if got := read(sam)["get_bundle"]; strings.Contains(got, "who owns the cache") {
					t.Errorf("sam's bundle serves a topic whose current stance they may not read: %s", got)
				}
			}
			var sole struct{ Stances []api.StanceRecord }
			if err := json.Unmarshal(w.http(t, kyle, "stance_history", map[string]any{"topic": alone.ID}), &sole); err != nil || len(sole.Stances) != 1 {
				t.Errorf("owner's history of the conversation's topic = %+v, %v", sole, err)
			}
		})
	}
}

func conversationDoc(t *testing.T, w *world, thread, exchange string, hour int, acl connector.ACL) string {
	t.Helper()
	at := day.Add(time.Duration(hour) * time.Hour)
	artifact := thread + "/" + exchange + ":owner"
	nativeID := artifact + "@r1"
	ev := connector.Event{
		Source: w.src, NativeID: nativeID, Kind: l1.KindConversationMessage, Time: at,
		Payload: connector.Payload{
			Artifact: artifact, BaseKind: connector.KindMessage, Thread: thread, Text: "Use the platform team's cache.",
			Container: connector.Container{Kind: "stream", NativeID: kyleNode},
			Author:    &connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: kyleNode},
			Revision:  &connector.Revision{Token: "r1"},
		},
		ACL: acl,
	}
	if _, err := l0.New(w.pool).Append(t.Context(), ev); err != nil {
		t.Fatalf("Append(%s) = %v", nativeID, err)
	}
	key := l1.ConversationSegmentPrefix(thread) + "feedfeedfeedfeedfeedfeed"
	doc := l1.Document{
		ID: l1.DocID(w.src, key), Kind: l1.KindConversationSegment, ArtifactClass: config.ArtifactDM,
		Source: l1.Source{System: w.src, NativeID: key},
		L0Refs: []string{connector.EventID(w.src, nativeID)},
		Time:   l1.Times{Created: at, Updated: at, LastActivity: at},
		Scope:  []string{w.scope, "code:" + w.project},
		ACL:    acl, RawText: "Owner: Use the platform team's cache.",
		Text: "The owner chose the platform team's cache.",
		Body: l1.Body{Summary: "The owner chose the platform team's cache.", Outcome: "The platform team owns the cache.", OutcomeKind: l1.OutcomeDecided},
	}
	if _, err := l1.New(w.pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	return doc.ID
}

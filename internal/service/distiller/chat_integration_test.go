//go:build integration

package distiller_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func chatMessage(src, id, body string, when time.Time) connector.Event {
	ev := event(src, connector.KindMessage, id, when, who(src, "u1", "kpenfound"), "", body)
	ev.Payload.Container = connector.Container{Kind: connector.ContainerChannel, NativeID: "C1", Name: "team"}
	ev.Payload.URL = "https://discord.com/channels/g/C1/" + id
	return ev
}

func chatRepo(src string) config.Repo {
	r := testRepo(src)
	r.Scopes[0].Sources[0].Containers = []string{"C1"}
	return r
}

func chatDistiller(t *testing.T, pool *pgxpool.Pool, src string, docs []l1.Document, answers []map[string]any) *distiller.Distiller {
	t.Helper()
	r := chatRepo(src)
	fx := loadFixtures(t)
	registry, err := llm.NewFake(r.LLM, fx)
	if err != nil {
		t.Fatal(err)
	}
	completer, err := registry.Completer(llm.TierDistill)
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := r.LLM.Tier(llm.TierDistill)
	for i, doc := range docs {
		want, err := json.Marshal(answers[i])
		if err != nil {
			t.Fatal(err)
		}
		response, err := completer.Complete(t.Context(), distiller.RequestFor(doc, budget.MaxTokens))
		var gotAnswer, wantAnswer any
		if err == nil {
			err = json.Unmarshal(response.JSON, &gotAnswer)
		}
		if err == nil {
			err = json.Unmarshal(want, &wantAnswer)
		}
		if err != nil || !reflect.DeepEqual(gotAnswer, wantAnswer) {
			t.Fatalf("the recorded chat answer for %s = %s, %v; want %s", doc.ID, response.JSON, err, want)
		}
	}
	cfg := config.Default()
	cfg.Repo = r
	d, err := distiller.New(pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestChatConversationsDistil(t *testing.T) {
	for _, tt := range []struct {
		name    string
		native  bool
		parent  bool
		outcome string
	}{
		{"native thread", true, false, "decided"},
		{"reply chain", false, true, "none"},
		{"channel window", false, false, "none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := newPool(t)
			src := newSource(t)
			first := chatMessage(src, "m1", "Should we ship this?", day)
			second := chatMessage(src, "m2", "Let us discuss it.", day.Add(5*time.Minute))
			var doc l1.Document
			var id string
			var err error
			if tt.native {
				root := chatMessage(src, "th1", "", day)
				root.Kind = connector.KindThread
				root.Payload.Title = "Release discussion"
				first.Payload.Thread, second.Payload.Thread = "th1", "th1"
				ingest(t, pool, []connector.Event{root, first, second})
				id = l1.DocID(src, "th1")
				resolver, _ := chatRepo(src).Resolver()
				doc, err = l1.Build(l1.Input{Root: root, Children: []connector.Event{first, second}, Repo: chatRepo(src), Resolver: resolver})
			} else {
				if tt.parent {
					second.Payload.Parent = "m1"
				}
				ingest(t, pool, []connector.Event{first, second})
				id = l1.DocID(src, l1.ChatWindowKey(first))
				resolver, _ := chatRepo(src).Resolver()
				doc, err = l1.BuildChatWindow(l1.ChatWindowKey(first), []connector.Event{first, second}, resolver, chatRepo(src))
			}
			if err != nil {
				t.Fatal(err)
			}
			answer := map[string]any{"summary": "A short release discussion.", "question": "Should we ship this?", "outcome_kind": tt.outcome}
			if tt.outcome == "decided" {
				answer["outcome"] = "Ship the release."
			}
			d := chatDistiller(t, pool, src, []l1.Document{doc}, []map[string]any{answer})
			result, err := d.Distill(t.Context(), id)
			if err != nil || !result.Written {
				t.Fatalf("Distill = %+v, %v", result, err)
			}
			if result.Asserting != (tt.outcome == "decided") {
				t.Errorf("Asserting = %v", result.Asserting)
			}
			stored, err := l1.New(pool).Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Kind != l1.KindChatThread || !slices.Equal(stored.L0Refs, doc.L0Refs) || stored.Source.URL == "" {
				t.Errorf("document = %+v", stored)
			}
			again, err := d.Distill(t.Context(), id)
			if err != nil || again.Written {
				t.Errorf("repeat = %+v, %v", again, err)
			}
		})
	}
}

func TestLateChatMessageKeepsWindowKeyAndDeletionRemovesMember(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	first := chatMessage(src, "m2", "Later message", day.Add(10*time.Minute))
	late := chatMessage(src, "m1", "Earlier message", day.Add(time.Minute))
	key := l1.ChatWindowKey(first)
	if key != l1.ChatWindowKey(late) {
		t.Fatal("late message moved windows")
	}
	ingest(t, pool, []connector.Event{first})
	resolver, _ := chatRepo(src).Resolver()
	one, _ := l1.BuildChatWindow(key, []connector.Event{first}, resolver, chatRepo(src))
	two, _ := l1.BuildChatWindow(key, []connector.Event{late, first}, resolver, chatRepo(src))
	edited := revised(first, "edit-1", day.Add(15*time.Minute))
	edited.Payload.Text = "Later message, edited"
	three, _ := l1.BuildChatWindow(key, []connector.Event{late, edited}, resolver, chatRepo(src))
	four, _ := l1.BuildChatWindow(key, []connector.Event{edited}, resolver, chatRepo(src))
	d := chatDistiller(t, pool, src, []l1.Document{one, two, three, four}, []map[string]any{
		{"summary": "Later message.", "outcome_kind": "none"},
		{"summary": "Two messages.", "outcome_kind": "none"},
		{"summary": "Two messages after an edit.", "outcome_kind": "none"},
		{"summary": "Edited message remains.", "outcome_kind": "none"},
	})
	id := l1.DocID(src, key)
	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	ingest(t, pool, []connector.Event{late})
	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stored, err := l1.New(pool).Get(t.Context(), id)
	if err != nil || !slices.Equal(stored.L0Refs, two.L0Refs) {
		t.Fatalf("after late message: %+v, %v", stored, err)
	}
	ingest(t, pool, []connector.Event{edited})
	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stored, err = l1.New(pool).Get(t.Context(), id)
	if err != nil || !slices.Equal(stored.L0Refs, three.L0Refs) {
		t.Fatalf("after edit: %+v, %v", stored, err)
	}
	tombstone := chatMessage(src, "m1:tombstone", "", day.Add(20*time.Minute))
	tombstone.Kind = connector.KindTombstone
	tombstone.Payload.Target = "m1"
	tombstone.Payload.Author = nil
	ingest(t, pool, []connector.Event{tombstone})
	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stored, err = l1.New(pool).Get(t.Context(), id)
	if err != nil || !slices.Equal(stored.L0Refs, four.L0Refs) {
		t.Fatalf("after deletion: %+v, %v", stored, err)
	}
	lastTombstone := chatMessage(src, "m2:tombstone", "", day.Add(25*time.Minute))
	lastTombstone.Kind = connector.KindTombstone
	lastTombstone.Payload.Target = "m2"
	lastTombstone.Payload.Author = nil
	ingest(t, pool, []connector.Event{lastTombstone})
	result, err := d.Distill(t.Context(), id)
	if err != nil || !result.Deleted {
		t.Fatalf("empty window: %+v, %v", result, err)
	}
}

func TestPrivateChatWindowKeepsACLAndUnresolvedMentionOut(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	first := chatMessage(src, "m1", "Can @unknown review this?", day)
	second := chatMessage(src, "m2", "I can review.", day.Add(3*time.Minute))
	group := connector.ACLEntry{Kind: connector.ACLGroup, Source: src, NativeID: "C1"}
	for _, ev := range []*connector.Event{&first, &second} {
		ev.ACL = connector.ACL{group}
		ev.Payload.Mentions = []connector.Identity{{Source: src, Kind: connector.IdentityUser, NativeID: "unmapped", Handle: "unknown"}}
	}
	ingest(t, pool, []connector.Event{first, second})
	key := l1.ChatWindowKey(first)
	resolver, _ := chatRepo(src).Resolver()
	doc, err := l1.BuildChatWindow(key, []connector.Event{first, second}, resolver, chatRepo(src))
	if err != nil {
		t.Fatal(err)
	}
	d := chatDistiller(t, pool, src, []l1.Document{doc}, []map[string]any{{"summary": "A review was requested.", "outcome_kind": "none"}})
	if _, err := d.Distill(t.Context(), l1.DocID(src, key)); err != nil {
		t.Fatal(err)
	}
	stored, err := l1.New(pool).Get(t.Context(), l1.DocID(src, key))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ACL) != 1 || stored.ACL[0].Kind != connector.ACLGroup || stored.ACL[0].NativeID != "C1" {
		t.Errorf("ACL = %+v", stored.ACL)
	}
	for _, p := range stored.Participants {
		if p.PrincipalID == "unknown" || p.PrincipalID == "unmapped" {
			t.Errorf("invented participant: %+v", p)
		}
	}
	for _, ref := range stored.References {
		if ref.ID == "unknown" || ref.ID == "unmapped" {
			t.Errorf("invented reference: %+v", ref)
		}
	}
	outside := l1.Reader{Effective: principal.Effective{Human: "sam", Grant: principal.Grant{Scopes: principal.SomeScopes("code:acme/api:engine")}}}
	inside := outside
	inside.Audience = []connector.ACLEntry{group}
	for _, tt := range []struct {
		name   string
		reader l1.Reader
		want   int
	}{{"outside", outside, 0}, {"inside", inside, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			visible, err := l1.New(pool).ListFor(t.Context(), tt.reader, l1.ListOptions{Source: src})
			if err != nil || len(visible) != tt.want {
				t.Errorf("ListFor = %d documents, %v; want %d", len(visible), err, tt.want)
			}
		})
	}
}

func TestChatPumpCollapsesMessagesInOneWindow(t *testing.T) {
	pool := newPool(t)
	pump := distiller.NewPump(pool, distiller.PumpOptions{Batch: l0.MaxLimit})
	client, err := queue.New(pool, queue.Config{Kind: distiller.JobKind()})
	if err != nil {
		t.Fatal(err)
	}
	for {
		progress, err := pump.Once(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if progress.Events < l0.MaxLimit {
			break
		}
	}
	before := pendingTargets(t, client)
	src := newSource(t)
	ingest(t, pool, []connector.Event{
		chatMessage(src, "m1", "First", day),
		chatMessage(src, "m2", "Second", day.Add(2*time.Minute)),
		chatMessage(src, "m3", "Third", day.Add(4*time.Minute)),
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := pump.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
		targets := fromSource(newTargets(pendingTargets(t, client), before), src)
		if len(targets) == 1 {
			if targets[0] != l1.DocID(src, l1.ChatWindowKey(chatMessage(src, "m1", "", day))) {
				t.Errorf("target = %v", targets)
			}
			return
		}
		if len(targets) > 1 || time.Now().After(deadline) {
			t.Fatalf("pump enqueued %v, want one chat window job", targets)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

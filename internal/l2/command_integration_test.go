//go:build integration

package l2_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// commandWorld is a chat source over a scope of its own: a distilled thread
// anyone may read, one only kyle may read, and topics to merge — one only
// kyle may read, and one under another scope key.
//
// kyle, sam and viv are people; kyle and sam may ratify by hand and viv may
// not. shed is an agent. User 15 is claimed by both amy and bea. User 99 maps
// to nobody.
type commandWorld struct {
	src, scope                string
	thread, private           string
	lock, holder, vault, away string
	pool                      *pgxpool.Pool
	commands                  *l2.Commands
}

func newCommandWorld(t *testing.T) *commandWorld {
	t.Helper()
	pool := newPool(t)
	ctx := t.Context()
	w := &commandWorld{src: unique(), pool: pool}
	w.scope = "d" + w.src
	w.thread, w.private = "C1/"+unique(), "C1/"+unique()

	root := t.TempDir()
	files := map[string]string{
		"sources/chat.yaml": fmt.Sprintf("id: %s\ntype: chat\ncontainers: [C1]\n", w.src),
		"scopes/s.yaml":     fmt.Sprintf("id: %s\nsources: [%s]\n", w.scope, w.src),
		"principals/p.yaml": fmt.Sprintf("- id: kyle\n  identities: [{source: %[1]s, native_id: \"11\"}]\n"+
			"- id: sam\n  identities: [{source: %[1]s, native_id: \"12\"}]\n"+
			"- id: viv\n  identities: [{source: %[1]s, native_id: \"13\"}]\n"+
			"- id: shed\n  kind: agent\n  class: worker\n  scopes: [code:%[1]s]\n  identities: [{source: %[1]s, native_id: \"14\"}]\n"+
			"- id: amy\n  identities: [{source: %[1]s, native_id: \"15\"}]\n"+
			"- id: bea\n  identities: [{source: %[1]s, handle: bea}]\n", w.src),
		"authority/a.yaml": ratifiers,
	}
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}

	public := connector.ACL{{Kind: connector.ACLPublic}}
	kyleOnly := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: "11"}}
	when := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	docs, graph := l1.New(pool), l2.New(pool)
	entity := "code:" + w.src
	if err := graph.PutEntity(ctx, l2.Entity{ID: entity, Type: l2.TypeProject, Name: w.src, Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	put := func(native string, kind l1.Kind, class config.ArtifactClass, acl connector.ACL) string {
		t.Helper()
		doc := l1.Document{ID: l1.DocID(w.src, native), Kind: kind, ArtifactClass: class, Source: l1.Source{System: w.src, NativeID: native},
			L0Refs: []string{connector.EventID(w.src, native)}, Time: l1.Times{Created: when, Updated: when, LastActivity: when}, Scope: []string{entity},
			ACL: acl, Text: native, RawText: native, Body: l1.Body{Summary: native, OutcomeKind: l1.OutcomeDecided}}
		if _, err := docs.Put(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	put(w.thread, l1.KindChatThread, config.ArtifactChatThread, public)
	put(w.private, l1.KindChatThread, config.ArtifactChatThread, kyleOnly)
	open := func(scope, name string, acl connector.ACL) string {
		t.Helper()
		doc := put(strings.ReplaceAll(name, " ", "-")+"-"+unique(), l1.KindIssue, config.ArtifactIssue, acl)
		topic := l2.Topic{ID: l2.TopicID(scope, doc, 0, name), Scope: scope, Name: name, ACL: acl, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		st := l2.Stance{ID: l2.StanceID(topic.ID, doc, name, when, l2.TierInferred), TopicID: topic.ID, Position: name,
			StatedAt: when, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: acl}
		if _, _, err := graph.AppendStance(ctx, st, when); err != nil {
			t.Fatal(err)
		}
		return topic.ID
	}
	w.lock = open(w.scope, "the lock", public)
	w.holder = open(w.scope, "who holds the lock", public)
	w.vault = open(w.scope, "the vault key", kyleOnly)
	w.away = open("source:"+w.src, "the lock elsewhere", public)

	if w.commands, err = l2.NewCommands(pool, repo); err != nil {
		t.Fatal(err)
	}
	return w
}

// user is a chat user as a source identity.
func (w *commandWorld) user(id string) connector.Identity {
	return connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: id}
}

func (w *commandWorld) request(user, verb string) connector.CommandRequest {
	return connector.CommandRequest{Source: w.src, Event: connector.EventID(w.src, "interaction:"+unique()), Invoker: w.user(user), Verb: verb}
}

// A command is applied as the person its invoker maps to, reading only what
// that person may read; every refusal is a distinguishable outcome and
// writes nothing.
func TestCommandsRefuseWithAReason(t *testing.T) {
	w := newCommandWorld(t)
	graph := l2.New(w.pool)
	pin := func(user, target string) connector.CommandRequest {
		req := w.request(user, connector.CommandPin)
		req.Target = target
		return req
	}
	merge := func(user, from, into string) connector.CommandRequest {
		req := w.request(user, connector.CommandMerge)
		req.From, req.Into = from, into
		return req
	}
	ambiguous := w.request("15", connector.CommandPin)
	ambiguous.Invoker.Handle = "bea"
	tests := []struct {
		name string
		req  connector.CommandRequest
		want connector.CommandResult
	}{
		{"a verb Hearsay does not have", w.request("11", "ratify"), connector.CommandResult{Outcome: connector.CommandUnknown}},
		{"an invoker mapped to nobody", pin("99", w.thread), connector.CommandResult{Outcome: connector.CommandUnmapped}},
		{"an invoker two principals claim", ambiguous, connector.CommandResult{Outcome: connector.CommandAmbiguous, Candidates: []string{"amy", "bea"}}},
		{"an agent's account", pin("14", w.thread), connector.CommandResult{Outcome: connector.CommandNotHuman, Principal: "shed", Kind: "agent"}},
		{"a pin outside a thread", pin("11", ""), connector.CommandResult{Outcome: connector.CommandNoTarget, Principal: "kyle", Kind: "human"}},
		{"a pin of a thread not distilled", pin("11", "C1/"+unique()), connector.CommandResult{Outcome: connector.CommandUndistilled, Principal: "kyle", Kind: "human"}},
		{"a pin of a thread the person may not read", pin("12", w.private), connector.CommandResult{Outcome: connector.CommandUndistilled, Principal: "sam", Kind: "human"}},
		{"a merge of a topic into itself", merge("12", w.lock, w.lock), connector.CommandResult{Outcome: connector.CommandSameTopic, Principal: "sam", Kind: "human"}},
		{"a merge of a topic the person may not read", merge("12", w.vault, w.lock), connector.CommandResult{Outcome: connector.CommandNoSuchTopic, Principal: "sam", Kind: "human", Topic: w.vault}},
		{"a merge of a topic that does not exist", merge("12", w.lock, "topic:nothing"), connector.CommandResult{Outcome: connector.CommandNoSuchTopic, Principal: "sam", Kind: "human", Topic: "topic:nothing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := w.commands.Apply(t.Context(), tt.req); !resultIs(got, tt.want) {
				t.Errorf("Apply() = %+v, want %+v", got, tt.want)
			}
		})
	}

	ledgerRefusals := []struct {
		name    string
		req     connector.CommandRequest
		outcome connector.CommandOutcome
		reason  string
	}{
		{"a pin by a person the scope does not let ratify", pin("13", w.thread), connector.CommandNotAllowed, `"viv" may not ratify by hand`},
		{"a merge by a person the scope does not let ratify", merge("13", w.holder, w.lock), connector.CommandNotAllowed, `"viv" may not ratify by hand`},
		{"a merge across scopes", merge("12", w.away, w.lock), connector.CommandRejected, ""},
	}
	for _, tt := range ledgerRefusals {
		t.Run(tt.name, func(t *testing.T) {
			got := w.commands.Apply(t.Context(), tt.req)
			if got.Outcome != tt.outcome || got.Reason == "" || !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("Apply() = %+v, want %s saying %q", got, tt.outcome, tt.reason)
			}
		})
	}

	if pins, err := graph.Pins(t.Context(), "code:"+w.src); err != nil || len(pins) != 0 {
		t.Errorf("pins after refusals = %v, %v; want none", pins, err)
	}
	if ops, err := graph.Operations(t.Context(), l2.OperationFilter{Scope: w.scope}); err != nil || len(ops) != 0 {
		t.Errorf("operations after refusals = %v, %v; want none", ops, err)
	}
}

// resultIs compares the fields a refusal sets.
func resultIs(got, want connector.CommandResult) bool {
	return got.Outcome == want.Outcome && got.Principal == want.Principal && got.Kind == want.Kind &&
		got.Topic == want.Topic && slices.Equal(got.Candidates, want.Candidates) && got.Gesture == 0 && got.Operation == 0
}

// A pin records a gesture keyed by the command event, so the same command
// applied again records nothing; a merge records an operation and names both
// topics.
func TestCommandsPinAndMerge(t *testing.T) {
	w := newCommandWorld(t)
	graph := l2.New(w.pool)

	req := w.request("11", connector.CommandPin)
	req.Target = w.private
	got := w.commands.Apply(t.Context(), req)
	g, err := graph.GestureByEvent(t.Context(), req.Event)
	if err != nil {
		t.Fatalf("kyle's pin (%+v) recorded no gesture: %v", got, err)
	}
	if got.Outcome != connector.CommandPinned || got.Scope == "" || got.Scope != g.Scope || got.Gesture != g.ID || got.Principal != "kyle" || !got.Applied() {
		t.Errorf("Apply(pin) = %+v, want pinned as gesture %d in %s", got, g.ID, g.Scope)
	}
	if g.Action != l2.GesturePin || g.Principal != "kyle" || !slices.Equal(g.Documents, []string{l1.DocID(w.src, w.private)}) {
		t.Errorf("gesture %+v, want kyle pinning the thread", g)
	}
	again := w.commands.Apply(t.Context(), req)
	if again.Outcome != connector.CommandAlreadyPinned || again.Gesture != g.ID || !again.Applied() {
		t.Errorf("Apply(the same pin) = %+v, want already pinned as gesture %d", again, g.ID)
	}
	if pins, err := graph.Pins(t.Context(), "code:"+w.src); err != nil || len(pins) != 1 {
		t.Errorf("pins = %v, %v; want the one", pins, err)
	}

	merge := w.request("12", connector.CommandMerge)
	merge.From, merge.Into = w.holder, w.lock
	got = w.commands.Apply(t.Context(), merge)
	ops, err := graph.Operations(t.Context(), l2.OperationFilter{Scope: w.scope})
	if err != nil || len(ops) != 1 {
		t.Fatalf("sam's merge (%+v) recorded %v, %v", got, ops, err)
	}
	want := connector.CommandResult{Outcome: connector.CommandMerged, Principal: "sam", Kind: "human", Scope: w.scope, Operation: ops[0].ID,
		FromName: "who holds the lock", IntoName: "the lock"}
	if got.Outcome != want.Outcome || got.Principal != want.Principal || got.Scope != want.Scope || got.Operation != want.Operation ||
		got.FromName != want.FromName || got.IntoName != want.IntoName {
		t.Errorf("Apply(merge) = %+v, want %+v", got, want)
	}
	if ops[0].Kind != l2.OperationMerge || ops[0].Principal != "sam" {
		t.Errorf("operation %+v, want sam's merge", ops[0])
	}
}

// Merge choices are the topics the invoker's person may read, through the
// same view a command reads through.
func TestMergeChoicesAreWhatThePersonMayRead(t *testing.T) {
	w := newCommandWorld(t)
	ids := func(choices []l2.TopicChoice) []string {
		out := []string{}
		for _, c := range choices {
			out = append(out, c.ID)
		}
		slices.Sort(out)
		return out
	}
	tests := []struct {
		name string
		req  l2.ChoiceRequest
		want []string
	}{
		{"sam sees the topics sam may read", l2.ChoiceRequest{Invoker: w.user("12")}, sortedIDs(w.lock, w.holder, w.away)},
		{"kyle also sees the one only kyle may read", l2.ChoiceRequest{Invoker: w.user("11")}, sortedIDs(w.lock, w.holder, w.vault, w.away)},
		{"what is typed narrows the list", l2.ChoiceRequest{Invoker: w.user("12"), Typed: " HOLDS "}, []string{w.holder}},
		{"into stays in from's scope and leaves from out", l2.ChoiceRequest{Invoker: w.user("12"), From: w.lock, Typed: "lock"}, []string{w.holder}},
		{"into after an unreadable from is every readable topic", l2.ChoiceRequest{Invoker: w.user("12"), From: w.vault}, sortedIDs(w.lock, w.holder, w.away)},
		{"an invoker mapped to nobody is offered nothing", l2.ChoiceRequest{Invoker: w.user("99")}, []string{}},
		{"an agent is offered nothing", l2.ChoiceRequest{Invoker: w.user("14")}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ids(w.commands.MergeChoices(t.Context(), tt.req)); !slices.Equal(got, tt.want) {
				t.Errorf("MergeChoices() = %v, want %v", got, tt.want)
			}
		})
	}
	for _, c := range w.commands.MergeChoices(t.Context(), l2.ChoiceRequest{Invoker: w.user("12"), Typed: "holds"}) {
		if c.Name != "who holds the lock" || c.Scope != w.scope {
			t.Errorf("choice %+v, want the holder's name and scope", c)
		}
	}
}

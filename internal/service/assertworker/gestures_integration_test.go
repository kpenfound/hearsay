//go:build integration

package assertworker_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func TestDiscordReactionGestures(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	const channel, messageID, userID = "123456789012345678", "123456789012345679", "123456789012345680"
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	acl := connector.ACL{{Kind: connector.ACLPublic}}
	container := connector.Container{Kind: connector.ContainerChannel, NativeID: channel}
	repo := config.Repo{
		Sources: []connector.SourceConfig{{ID: src, Type: discord.Type, Settings: json.RawMessage(`{"guild":"123456789012345677","ratify_emoji":"👍","demote_emoji":"👎"}`)}},
		Scopes:  []config.Scope{{ID: src, Sources: []config.ScopeSource{{Source: src, Containers: []string{channel}}}}},
		Principals: []principal.Principal{
			{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: src, NativeID: userID}}},
			{ID: "bot", Kind: principal.KindAgent, Identities: []principal.Identity{{Source: src, NativeID: "agent-user"}}},
		},
	}
	message := connector.Event{Source: src, NativeID: messageID + "@v1", Kind: connector.KindMessage, Time: at, ACL: acl,
		Payload: connector.Payload{Artifact: messageID, Container: container, Text: "a message", Author: &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: userID}, Revision: &connector.Revision{Token: "v1"}}}
	if _, err := l0.New(pool).Append(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	doc := l1.Document{ID: l1.DocID(src, "thread"), Kind: l1.KindChatThread, ArtifactClass: config.ArtifactChatThread,
		Source: l1.Source{System: src, NativeID: "thread"}, L0Refs: []string{connector.EventID(src, message.NativeID)},
		Time: l1.Times{Created: at, Updated: at, LastActivity: at}, Scope: []string{"chat"}, ACL: acl,
		Text: "a message", RawText: "a message", Body: l1.Body{Summary: "a message", OutcomeKind: l1.OutcomeDecided}}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	graph := l2.New(pool)
	for i := range 2 {
		name := []string{"choice one", "choice two"}[i]
		topic := l2.Topic{ID: l2.TopicID(src, doc.ID, i, name), Scope: src, Name: name, ACL: acl, OpenedBy: doc.ID}
		if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
			t.Fatal(err)
		}
		if _, _, err := graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(topic.ID, doc.ID, name, at, l2.TierInferred), TopicID: topic.ID,
			Position: name, StatedAt: at, Evidence: []string{doc.ID}, Tier: l2.TierInferred, ACL: acl}, at); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Repo = repo
	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	a, err := assertworker.New(pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	appendReaction := func(emoji, user string) connector.Event {
		t.Helper()
		artifact := messageID + ":reaction:" + user + ":" + emoji
		ev := connector.Event{Source: src, NativeID: artifact, Kind: connector.KindReaction, Time: at, ACL: acl,
			Payload: connector.Payload{Artifact: artifact, Container: container, Parent: messageID,
				Author: &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: user}, Native: json.RawMessage(`{"emoji":"` + emoji + `"}`)}}
		if _, err := l0.New(pool).Append(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
		ev.ID = connector.EventID(src, ev.NativeID)
		return ev
	}
	handle := func(ev connector.Event) {
		t.Helper()
		if err := a.Handle(t.Context(), queue.Job{Kind: l2.AssertKind(), TargetID: "gesture:" + ev.ID, SerialKey: src}); err != nil {
			t.Fatal(err)
		}
	}
	ignored := appendReaction("🤔", userID)
	handle(ignored)
	unknown := appendReaction("👎", "unknown")
	handle(unknown)
	unauthorized := appendReaction("👎", "agent-user")
	handle(unauthorized)
	if gs, err := graph.Gestures(t.Context(), src); err != nil || len(gs) != 0 {
		t.Fatalf("ignored gestures = %+v, %v", gs, err)
	}
	ratify := appendReaction("👍", userID)
	handle(ratify)
	handle(ratify)
	gs, err := graph.Gestures(t.Context(), src)
	if err != nil || len(gs) != 1 || gs[0].Action != l2.GestureRatify || len(gs[0].Stances) != 2 || gs[0].Documents[0] != doc.ID {
		t.Fatalf("ratify = %+v, %v", gs, err)
	}
	tombstone := connector.Event{Source: src, NativeID: ratify.NativeID + ":tombstone", Kind: connector.KindTombstone, Time: at, ACL: acl,
		Payload: connector.Payload{Artifact: ratify.NativeID + ":tombstone", Target: ratify.NativeID, Container: container}}
	if _, err := l0.New(pool).Append(t.Context(), tombstone); err != nil {
		t.Fatal(err)
	}
	tombstone.ID = connector.EventID(src, tombstone.NativeID)
	handle(tombstone)
	handle(tombstone)
	gs, err = graph.Gestures(t.Context(), src)
	if err != nil || len(gs) != 2 || gs[1].Action != l2.GestureUndo || gs[0].UndoneBy != gs[1].ID {
		t.Fatalf("undo = %+v, %v", gs, err)
	}
	demote := appendReaction("👎", userID)
	handle(demote)
	gs, err = graph.Gestures(t.Context(), src)
	if err != nil || len(gs) != 3 || gs[2].Action != l2.GestureDemote {
		t.Fatalf("demote = %+v, %v", gs, err)
	}
	repo.Sources[0].Settings = json.RawMessage(`{"guild":"123456789012345677"}`)
	cfg.Repo = repo
	a, err = assertworker.New(pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defaultRatify := appendReaction("✅", userID)
	handle(defaultRatify)
	gs, err = graph.Gestures(t.Context(), src)
	if err != nil || len(gs) != 4 || gs[3].Action != l2.GestureRatify {
		t.Fatalf("default ratify = %+v, %v", gs, err)
	}
	follower := assertworker.NewGestureFollower(pool, repo)
	if _, err := follower.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		target string
		want   int
	}{
		{"gesture:" + ratify.ID, 0}, // L0's feed hides a reaction already tombstoned.
		{"gesture:" + tombstone.ID, 1},
		{"gesture:" + connector.EventID(src, message.NativeID), 0},
	} {
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE target_id = $1`, tc.target).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != tc.want {
			t.Errorf("queued %s = %d, want %d", tc.target, count, tc.want)
		}
	}
	if _, err := follower.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// A read-only Discord source's reactions are ingested and are not gestures:
// the follower enqueues nothing for them, a job for one enqueued before the
// source became read-only records nothing, and removing a reaction that was
// recorded as a gesture before then undoes nothing. The same reactions on a
// source that is not read-only are TestDiscordReactionGestures.
func TestReadOnlyDiscordReactionsAreNotGestures(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	const channel, messageID, userID = "123456789012345678", "123456789012345679", "123456789012345680"
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	acl := connector.ACL{{Kind: connector.ACLPublic}}
	container := connector.Container{Kind: connector.ContainerChannel, NativeID: channel}
	repo := config.Repo{
		Sources:    []connector.SourceConfig{{ID: src, Type: discord.Type, Settings: json.RawMessage(`{"guild":"123456789012345677"}`)}},
		Scopes:     []config.Scope{{ID: src, Sources: []config.ScopeSource{{Source: src, Containers: []string{channel}}}}},
		Principals: []principal.Principal{{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: src, NativeID: userID}}}},
	}
	readOnly := repo
	readOnly.Sources = slices.Clone(repo.Sources)
	readOnly.Sources[0].ReadOnly = true

	events := l0.New(pool)
	message := connector.Event{Source: src, NativeID: messageID + "@v1", Kind: connector.KindMessage, Time: at, ACL: acl,
		Payload: connector.Payload{Artifact: messageID, Container: container, Text: "a message", Author: &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: userID}, Revision: &connector.Revision{Token: "v1"}}}
	if _, err := events.Append(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	doc := l1.Document{ID: l1.DocID(src, "thread"), Kind: l1.KindChatThread, ArtifactClass: config.ArtifactChatThread,
		Source: l1.Source{System: src, NativeID: "thread"}, L0Refs: []string{connector.EventID(src, message.NativeID)},
		Time: l1.Times{Created: at, Updated: at, LastActivity: at}, Scope: []string{"chat"}, ACL: acl,
		Text: "a message", RawText: "a message", Body: l1.Body{Summary: "a message", OutcomeKind: l1.OutcomeDecided}}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	graph := l2.New(pool)
	topic := l2.Topic{ID: l2.TopicID(src, doc.ID, 0, "a choice"), Scope: src, Name: "a choice", ACL: acl, OpenedBy: doc.ID}
	if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
		t.Fatal(err)
	}
	if _, _, err := graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(topic.ID, doc.ID, "a choice", at, l2.TierInferred), TopicID: topic.ID,
		Position: "a choice", StatedAt: at, Evidence: []string{doc.ID}, Tier: l2.TierInferred, ACL: acl}, at); err != nil {
		t.Fatal(err)
	}
	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	asserter := func(repo config.Repo) *assertworker.Asserter {
		t.Helper()
		cfg := config.Default()
		cfg.Repo = repo
		a, err := assertworker.New(pool, registry, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	appendEvent := func(ev connector.Event) (connector.Event, l0.Cursor) {
		t.Helper()
		appended, err := events.Append(t.Context(), ev)
		if err != nil {
			t.Fatal(err)
		}
		ev.ID = appended.ID
		return ev, appended.Cursor
	}
	reaction := func(emoji string) connector.Event {
		artifact := messageID + ":reaction:" + userID + ":" + emoji
		return connector.Event{Source: src, NativeID: artifact, Kind: connector.KindReaction, Time: at, ACL: acl,
			Payload: connector.Payload{Artifact: artifact, Container: container, Parent: messageID,
				Author: &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: userID}, Native: json.RawMessage(`{"emoji":"` + emoji + `"}`)}}
	}
	handle := func(a *assertworker.Asserter, ev connector.Event) {
		t.Helper()
		if err := a.Handle(t.Context(), queue.Job{Kind: l2.AssertKind(), TargetID: l2.GestureTarget + ev.ID, SerialKey: src}); err != nil {
			t.Fatal(err)
		}
	}

	// Recorded while the source was not read-only.
	before, _ := appendEvent(reaction("✅"))
	handle(asserter(repo), before)
	gs, err := graph.Gestures(t.Context(), src)
	if err != nil || len(gs) != 1 || gs[0].Action != l2.GestureRatify {
		t.Fatalf("gestures before the source is read-only = %+v, %v, want one ratify", gs, err)
	}

	a := asserter(readOnly)
	demote, _ := appendEvent(reaction("👎"))
	handle(a, demote)
	tombstone, last := appendEvent(connector.Event{Source: src, NativeID: before.NativeID + ":tombstone", Kind: connector.KindTombstone, Time: at, ACL: acl,
		Payload: connector.Payload{Artifact: before.NativeID + ":tombstone", Target: before.NativeID, Container: container}})
	handle(a, tombstone)
	gs, err = graph.Gestures(t.Context(), src)
	if err != nil || len(gs) != 1 || gs[0].UndoneBy != 0 {
		t.Errorf("gestures = %+v, %v, want only the ratify from before, not undone", gs, err)
	}

	readThrough(t, pool, "assert-worker:gestures", assertworker.NewGestureFollower(pool, readOnly).Once, last)
	var queued int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE target_id LIKE 'gesture:%'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("queued %d gesture jobs for a read-only source, want none", queued)
	}
}

// readThrough runs a feed follower until its cursor is last, the cursor of the
// last event appended. The feed holds events back while any transaction in
// the cluster is open, so one read may return nothing.
func readThrough(t *testing.T, pool *pgxpool.Pool, consumer string, once func(context.Context) (int, error), last l0.Cursor) {
	t.Helper()
	waitFor(t, consumer+" to read through "+last.String(), func() bool {
		if _, err := once(t.Context()); err != nil {
			t.Fatalf("Once() = %v", err)
		}
		at, err := l0.NewCursors(pool).Load(t.Context(), consumer)
		if err != nil {
			t.Fatal(err)
		}
		return at == last
	})
}

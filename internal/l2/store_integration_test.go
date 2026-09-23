//go:build integration

package l2_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var uniques atomic.Int64

// unique is a suffix nothing else in the shared database uses.
func unique() string {
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(uniques.Add(1), 36)
}

// scratchPool is a migrated database of this test's own, for the one write that
// is global by design: replacing the seeded entities deletes every seeded row
// not in the set, which on the shared database would delete another package's.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := newPool(t)
	name := "hearsay_l2_" + unique()
	if _, err := admin.Exec(t.Context(), `CREATE DATABASE `+name); err != nil {
		t.Fatalf("creating the scratch database: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := admin.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping the scratch database: %v", err)
		}
	})
	url := os.Getenv("HEARSAY_DATABASE_URL")
	base, query, hasQuery := strings.Cut(url, "?")
	url = base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		url += "?" + query
	}
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("NewMigrator() = %v", err)
	}
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("migrating the scratch database: %v", err)
	}
	_ = migrator.Close()
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to the scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var public = connector.ACL{{Kind: connector.ACLPublic}}

func TestEntitiesAreSeededAndCreatedOnReference(t *testing.T) {
	pool := scratchPool(t)
	store := l2.New(pool)
	ctx := t.Context()

	seeded := []l2.Entity{
		{ID: "code:acme/api:engine", Type: l2.TypeModule, Name: "engine", Aliases: []string{"the engine"}, Origin: l2.OriginConfig},
		{ID: "code:acme/api:gone", Type: l2.TypeModule, Name: "gone", Origin: l2.OriginConfig},
		{ID: "code:acme/api", Type: l2.TypeProject, Origin: l2.OriginRepoStructure},
	}
	if err := store.ReplaceSeeded(ctx, seeded); err != nil {
		t.Fatalf("ReplaceSeeded() = %v", err)
	}
	item := l2.Entity{ID: "tracker:github-acme:acme/api#12", Type: l2.TypeTrackerItem, Name: "acme/api#12", Origin: l2.OriginReference}
	if created, err := store.EnsureEntity(ctx, item); err != nil || !created {
		t.Fatalf("EnsureEntity() = %v, %v, want it created", created, err)
	}
	// A second reference changes nothing, and does not overwrite what is there.
	renamed := item
	renamed.Name = "something else"
	if created, err := store.EnsureEntity(ctx, renamed); err != nil || created {
		t.Fatalf("EnsureEntity(again) = %v, %v, want nothing written", created, err)
	}

	// Removing an entry from configuration removes its row, and never a row a
	// document created.
	if err := store.ReplaceSeeded(ctx, []l2.Entity{seeded[0], seeded[2]}); err != nil {
		t.Fatalf("ReplaceSeeded(without gone) = %v", err)
	}
	entities, err := store.Entities(ctx)
	if err != nil {
		t.Fatalf("Entities() = %v", err)
	}
	var ids []string
	for _, e := range entities {
		ids = append(ids, e.ID)
	}
	if want := []string{"code:acme/api", "code:acme/api:engine", item.ID}; !slices.Equal(ids, want) {
		t.Errorf("Entities() = %q, want %q", ids, want)
	}
	if e, err := store.Entity(ctx, item.ID); err != nil || e.Name != item.Name {
		t.Errorf("Entity(%s) = %+v, %v, want the first reference's row", item.ID, e, err)
	}
	if _, err := store.Entity(ctx, "code:acme/api:gone"); !errors.Is(err, l2.ErrNotFound) {
		t.Errorf("Entity(gone) = %v, want ErrNotFound", err)
	}
	person := l2.Entity{ID: "person:robin", Type: l2.TypePerson, Origin: l2.OriginReference}
	if err := store.ReplaceSeeded(ctx, []l2.Entity{seeded[0], seeded[2], person}); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("ReplaceSeeded(a reference that is no tracker item) = %v, want ErrInvalid", err)
	}

	matches, err := store.Resolve(ctx, "The Engine is slow, see acme/api#12")
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	var matched []string
	for _, m := range matches {
		matched = append(matched, m.Entity.ID)
	}
	if want := []string{"code:acme/api:engine", item.ID}; !slices.Equal(matched, want) {
		t.Errorf("Resolve() = %q, want %q", matched, want)
	}

	// Issue #120: the tracker items the tracker places are seeded with their
	// parents, and one it no longer places keeps its row and loses them.
	placed := item
	placed.PartOf = []string{"tracker:github-acme:acme/api#10"}
	if err := store.ReplaceSeeded(ctx, []l2.Entity{seeded[0], seeded[2], placed}); err != nil {
		t.Fatalf("ReplaceSeeded(a placed tracker item) = %v", err)
	}
	if e, err := store.Entity(ctx, item.ID); err != nil || !slices.Equal(e.PartOf, placed.PartOf) {
		t.Errorf("Entity(%s) = %+v, %v, want it part of #10", item.ID, e, err)
	}
	if err := store.ReplaceSeeded(ctx, []l2.Entity{seeded[0], seeded[2]}); err != nil {
		t.Fatalf("ReplaceSeeded(placed nowhere) = %v", err)
	}
	if e, err := store.Entity(ctx, item.ID); err != nil || len(e.PartOf) != 0 {
		t.Errorf("Entity(%s) = %+v, %v, want it kept and part of nothing", item.ID, e, err)
	}
}

func openTopic(t *testing.T, store *l2.Store, scope string, acl connector.ACL, keys ...string) l2.Topic {
	t.Helper()
	topic := l2.Topic{
		ID: l2.TopicID(scope, "l1:s:"+scope, 0, "the lock"), Scope: scope, Name: "the lock",
		JoinKeys: keys, ACL: acl, OpenedBy: "l1:s:" + scope,
	}
	if opened, err := store.OpenTopic(t.Context(), topic); err != nil || !opened {
		t.Fatalf("OpenTopic() = %v, %v", opened, err)
	}
	return topic
}

func stance(topic l2.Topic, doc, position string, hour int) l2.Stance {
	at := time.Date(2026, 9, 9, hour, 0, 0, 0, time.UTC)
	return l2.Stance{
		ID: l2.StanceID(topic.ID, doc, position, at, l2.TierInferred), TopicID: topic.ID, Position: position,
		StatedAt: at, Evidence: []string{doc},
		Tier: l2.TierInferred, ACL: public,
	}
}

func TestStancesAreSupersededNeverOverwritten(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	topic := openTopic(t, store, unique(), public)

	add := func(st l2.Stance) (l2.Stance, bool) {
		t.Helper()
		stored, written, err := store.AppendStance(ctx, st, st.StatedAt)
		if err != nil {
			t.Fatalf("AppendStance(%s) = %v", st.Position, err)
		}
		return stored, written
	}
	first, written := add(stance(topic, "l1:s:issue", "queue takes the lock", 1))
	if !written || first.Supersedes != "" {
		t.Fatalf("the first stance = %+v, written %v, want written and superseding nothing", first, written)
	}
	secondStance := stance(topic, "l1:s:pr", "engine takes the lock", 5)
	secondStance.Judgement = l2.JudgementChanges
	second, _ := add(secondStance)
	if second.Supersedes != first.ID {
		t.Errorf("the second stance supersedes %q, want %q", second.Supersedes, first.ID)
	}
	if first.Judgement != l2.JudgementUnknown || second.Judgement != l2.JudgementChanges {
		t.Errorf("stored judgements = %q, %q, want unknown then changes", first.Judgement, second.Judgement)
	}

	// The same position from the same document again is the stance already
	// there, as it was stored.
	again, written := add(stance(topic, "l1:s:pr", "engine takes the lock", 5))
	if written || again.ID != second.ID || again.Supersedes != second.Supersedes {
		t.Errorf("appending again = %+v, written %v, want the stored stance and nothing written", again, written)
	}

	// A document read late supersedes what came before it and leaves the later
	// stance alone: the chain forks rather than being rewritten.
	late, _ := add(stance(topic, "l1:s:thread", "nobody agrees yet", 3))
	if late.Supersedes != first.ID {
		t.Errorf("the late stance supersedes %q, want %q", late.Supersedes, first.ID)
	}
	if reread, err := store.Stance(ctx, second.ID); err != nil || reread.Supersedes != first.ID {
		t.Errorf("the later stance after a late one = %+v, %v, want it untouched", reread, err)
	}
	// And the newest stance supersedes the newest before it, of three.
	fourth, _ := add(stance(topic, "l1:s:later", "the engine keeps the lock", 6))
	if fourth.Supersedes != second.ID {
		t.Errorf("the newest stance supersedes %q, want the newest before it, %q", fourth.Supersedes, second.ID)
	}

	forged := stance(topic, "l1:s:other", "y", 7)
	forged.ID = "stance:chosen-by-the-caller"
	if _, _, err := store.AppendStance(ctx, forged, forged.StatedAt); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("AppendStance(an id not derived from the stance) = %v, want ErrInvalid", err)
	}

	history, err := store.StanceHistory(ctx, topic.ID)
	if err != nil {
		t.Fatalf("StanceHistory() = %v", err)
	}
	var positions []string
	for _, st := range history {
		positions = append(positions, st.Position)
	}
	if want := []string{"queue takes the lock", "nobody agrees yet", "engine takes the lock", "the engine keeps the lock"}; !slices.Equal(positions, want) {
		t.Errorf("StanceHistory() = %q, want %q", positions, want)
	}
	from, err := store.StancesFrom(ctx, "l1:s:pr")
	if err != nil || len(from) == 0 || from[len(from)-1].ID != second.ID {
		t.Errorf("StancesFrom(pr) = %+v, %v, want it to hold the pull request's stance", from, err)
	}

	preset := stance(topic, "l1:s:other", "x", 6)
	preset.Supersedes = first.ID
	if _, _, err := store.AppendStance(ctx, preset, preset.StatedAt); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("AppendStance(naming its predecessor) = %v, want ErrInvalid", err)
	}
}

// A document read again in a new version replaces its own stance on a topic
// rather than answering whatever is newest there (#72).
func TestANewReadingOfADocumentRetiresItsOwnStance(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	topic := openTopic(t, store, unique(), public)
	add := func(st l2.Stance) l2.Stance {
		t.Helper()
		stored, written, err := store.AppendStance(ctx, st, st.StatedAt)
		if err != nil || !written {
			t.Fatalf("AppendStance(%s) = %v, written %v", st.Position, err, written)
		}
		return stored
	}
	live := func() []string {
		t.Helper()
		history, err := store.StanceHistory(ctx, topic.ID)
		if err != nil {
			t.Fatalf("StanceHistory() = %v", err)
		}
		superseded := map[string]bool{}
		for _, st := range history {
			superseded[st.Supersedes] = true
		}
		var out []string
		for _, st := range history {
			if !superseded[st.ID] {
				out = append(out, st.Position)
			}
		}
		return out
	}

	// The #51 fixture plus a comment: the issue proposes, the merged pull
	// request does otherwise, and the issue, re-distilled after the merge,
	// restates its proposal in other words.
	s1 := add(stance(topic, "l1:s:issue", "move the lock into the queue", 1))
	s2 := add(stance(topic, "l1:s:pr", "the engine takes the lock", 3))
	s3 := add(stance(topic, "l1:s:issue", "the queue should hold the lock", 5))
	if s2.Supersedes != s1.ID {
		t.Errorf("the pull request's stance supersedes %q, want the issue's %q", s2.Supersedes, s1.ID)
	}
	if s3.Supersedes != s1.ID {
		t.Errorf("the issue's restatement supersedes %q, want the issue's own earlier stance %q", s3.Supersedes, s1.ID)
	}
	from, err := store.StancesFrom(ctx, "l1:s:issue")
	if err != nil {
		t.Fatalf("StancesFrom(issue) = %v", err)
	}
	if kept := slices.DeleteFunc(from, func(st l2.Stance) bool { return st.TopicID != topic.ID }); len(kept) != 2 {
		t.Errorf("StancesFrom(issue) on the topic = %+v, want both readings kept", kept)
	}
	if got, want := live(), []string{"the engine takes the lock", "the queue should hold the lock"}; !slices.Equal(got, want) {
		t.Errorf("unsuperseded stances = %q, want %q: one per document", got, want)
	}

	// A later document on the topic answers the head, never the retired stance.
	s4 := add(stance(topic, "l1:s:thread", "agreed, the engine", 6))
	if s4.Supersedes != s3.ID {
		t.Errorf("a later document's stance supersedes %q, want the head %q", s4.Supersedes, s3.ID)
	}

	// A reading stated earlier than the document's previous one — a deletion
	// took the comment that moved its last activity — still replaces it, and
	// the newer stance it retired is not the topic's current one.
	s5 := add(stance(topic, "l1:s:thread", "the engine, for now", 2))
	if s5.Supersedes != s4.ID {
		t.Errorf("a reading stated before its document's last supersedes %q, want that document's %q", s5.Supersedes, s4.ID)
	}
	history, err := store.StanceHistory(ctx, topic.ID)
	if err != nil {
		t.Fatalf("StanceHistory() = %v", err)
	}
	if current, ok := l2.Current(history); !ok || current.ID != s3.ID {
		t.Errorf("Current() = %+v, %v, want the issue's restatement: the thread's newer stance was retired", current, ok)
	}

	// A document stated after the retired stance skips it for the newest live
	// one.
	s6 := add(stance(topic, "l1:s:review", "ship it", 7))
	if s6.Supersedes != s3.ID {
		t.Errorf("a stance stated after a retired one supersedes %q, want the newest live one before it, %q", s6.Supersedes, s3.ID)
	}
}

// A stance an agent asserted cites a document without having been read from
// it: a later reading of that document replaces the document's own stance and
// leaves the agent's alone, and the assertion is written once however often it
// is appended.
func TestAnAssertedStanceIsNotTheDocumentItCites(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	topic := openTopic(t, store, unique(), public)
	add := func(st l2.Stance) (l2.Stance, bool) {
		t.Helper()
		stored, written, err := store.AppendStance(ctx, st, st.StatedAt)
		if err != nil {
			t.Fatalf("AppendStance(%s) = %v", st.Position, err)
		}
		return stored, written
	}
	event := connector.EventID(connector.SelfSource, "assertion:"+unique())
	asserted := func(position string, hour int) l2.Stance {
		st := stance(topic, "l1:s:issue", position, hour)
		st.ID, st.Assertion, st.Author = l2.AssertionStanceID(topic.ID, event), event, "shed"
		st.Evidence = []string{"l1:s:issue", "l1:s:pr"}
		return st
	}

	read, _ := add(stance(topic, "l1:s:issue", "move the lock into the queue", 1))
	agent, written := add(asserted("the engine should take the lock", 2))
	if !written || agent.Assertion != event || agent.Supersedes != read.ID || !slices.Equal(agent.Evidence, []string{"l1:s:issue", "l1:s:pr"}) {
		t.Fatalf("the asserted stance = %+v, written %v, want it written after the issue's with its event and evidence", agent, written)
	}
	if again, written := add(asserted("the engine should take the lock", 2)); written || again.ID != agent.ID {
		t.Errorf("appending the assertion again = %+v, written %v, want the stored stance and nothing written", again, written)
	}
	if from, err := store.StancesFrom(ctx, "l1:s:issue"); err != nil || slices.ContainsFunc(from, func(st l2.Stance) bool { return st.ID == agent.ID }) {
		t.Errorf("StancesFrom(issue) = %+v, %v, want the issue's own stances only", from, err)
	}

	// The issue read again replaces its own stance, not the agent's.
	reread, _ := add(stance(topic, "l1:s:issue", "the queue should hold the lock", 3))
	if reread.Supersedes != read.ID {
		t.Errorf("the issue's new reading supersedes %q, want its own earlier stance %q", reread.Supersedes, read.ID)
	}
	history, err := store.StanceHistory(ctx, topic.ID)
	if err != nil {
		t.Fatalf("StanceHistory() = %v", err)
	}
	standing, ok := l2.Stand(l2.TierInputs{History: history, Policy: config.DefaultPolicy()})
	if !ok {
		t.Fatal("Stand() found no live stance")
	}
	// Two live stances: the agent's and the issue's new reading. Neither has
	// evidence L1 holds here, so the agent's class is what ranks it — above a
	// document L1 does not hold.
	if standing.Current.ID != agent.ID || standing.Tier != l2.TierInferred {
		t.Errorf("Stand() = %s at %s, want the agent's stance, inferred: it is live and ranks as an agent", standing.Current.ID, standing.Tier)
	}

	forged := asserted("the engine should take the lock", 2)
	forged.ID = l2.StanceID(topic.ID, "l1:s:issue", forged.Position, forged.StatedAt, forged.Tier)
	if _, _, err := store.AppendStance(ctx, forged, forged.StatedAt); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("AppendStance(an asserted stance with a document's id) = %v, want ErrInvalid", err)
	}
}

func TestReturningToAnEarlierPositionWritesANewStance(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	topic := openTopic(t, store, unique(), public)
	doc := "l1:s:" + unique()
	positions := []string{"take X", "take X prime", "take X"}
	var history []l2.Stance
	for i, position := range positions {
		st := stance(topic, doc, position, i+1)
		got, written, err := store.AppendStance(t.Context(), st, st.StatedAt)
		if err != nil || !written {
			t.Fatalf("AppendStance(version %d) = %+v, %v, written %v", i+1, got, err, written)
		}
		history = append(history, got)
	}
	if history[2].ID == history[0].ID || history[2].Supersedes != history[1].ID {
		t.Errorf("return to X = %+v, want a new stance superseding %s", history[2], history[1].ID)
	}
	stored, err := store.StanceHistory(t.Context(), topic.ID)
	if err != nil || len(stored) != 3 {
		t.Fatalf("StanceHistory() = %+v, %v, want all three readings", stored, err)
	}
	if current, ok := l2.Current(stored); !ok || current.ID != history[2].ID {
		t.Errorf("Current() = %+v, %v, want the returned position", current, ok)
	}
	retry := stance(topic, doc, positions[2], 3)
	if again, written, err := store.AppendStance(t.Context(), retry, retry.StatedAt); err != nil || written || again.ID != history[2].ID {
		t.Errorf("retry of version 3 = %+v, %v, written %v, want original row", again, err, written)
	}
}

func TestTopicsAreOnlyOfferedToDocumentsTheirReadersMayRead(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	scope := unique()
	group := connector.ACLEntry{Kind: connector.ACLGroup, Source: "github-acme", NativeID: "acme/api", Label: "acme/api collaborators"}
	other := connector.ACLEntry{Kind: connector.ACLGroup, Source: "github-acme", NativeID: "acme/secret"}
	openTopicFrom(t, pool, scope, "acme/api#12", connector.ACL{group}, "item:acme/api#12")

	tests := []struct {
		name    string
		scope   string
		keys    []string
		readers connector.ACL
		want    int
	}{
		{"a document with the same grant, labelled differently", scope, []string{"item:acme/api#12"},
			connector.ACL{{Kind: connector.ACLGroup, Source: "github-acme", NativeID: "acme/api", Label: "a label the source changed"}}, 1},
		{"a public document is not shown a private topic", scope, []string{"item:acme/api#12"}, public, 0},
		{"a document readable more widely than the topic", scope, []string{"item:acme/api#12"}, connector.ACL{group, other}, 0},
		{"a document with another grant", scope, []string{"item:acme/api#12"}, connector.ACL{other}, 0},
		{"no shared key", scope, []string{"item:acme/api#13"}, connector.ACL{group}, 0},
		{"another scope", unique(), []string{"item:acme/api#12"}, connector.ACL{group}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.TopicsByJoinKeys(ctx, tt.scope, tt.keys, tt.readers, 5)
			if err != nil {
				t.Fatalf("TopicsByJoinKeys() = %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("TopicsByJoinKeys() = %d topics, want %d", len(got), tt.want)
			}
		})
	}

	// A public topic is offered to everyone, most shared keys first.
	more := openTopicFrom(t, pool, scope, "acme/api#31", public, "item:acme/api#12", "item:acme/api#31")
	got, err := store.TopicsByJoinKeys(ctx, scope, []string{"item:acme/api#12", "item:acme/api#31"}, connector.ACL{group}, 5)
	if err != nil || len(got) != 2 || got[0].ID != more.ID {
		t.Errorf("TopicsByJoinKeys(two keys) = %+v, %v, want both, the one sharing two keys first", got, err)
	}
}

// Issue #114: who a topic is offered to is its opening document's access list
// now, not the one the topic was written with.
func TestATopicIsOfferedByItsOpeningDocumentAsItIsNow(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	group := connector.ACL{{Kind: connector.ACLGroup, Source: "github-acme", NativeID: "acme/api"}}
	offered := func(scope string, readers connector.ACL) int {
		t.Helper()
		got, err := store.TopicsByJoinKeys(ctx, scope, []string{"item:acme/api#12"}, readers, 5)
		if err != nil {
			t.Fatalf("TopicsByJoinKeys() = %v", err)
		}
		return len(got)
	}

	narrowed := unique()
	topic := openTopicFrom(t, pool, narrowed, "acme/api#12", public, "item:acme/api#12")
	if n := offered(narrowed, public); n != 1 {
		t.Fatalf("a public topic was offered to %d public documents' readers, want 1", n)
	}
	putDoc(t, pool, narrowed, "acme/api#12", group, nil)
	if n := offered(narrowed, public); n != 0 {
		t.Errorf("a topic whose document went private is still offered to a public document")
	}
	if n := offered(narrowed, group); n != 1 {
		t.Errorf("a topic whose document went private is not offered to its own readers")
	}
	if kept, err := store.Topic(ctx, topic.ID); err != nil || len(kept.ACL) != 1 || kept.ACL[0].Kind != connector.ACLPublic {
		t.Errorf("Topic() = %+v, %v, want the access list it was written with kept as it was", kept, err)
	}

	retracted := unique()
	topic = openTopicFrom(t, pool, retracted, "acme/api#12", public, "item:acme/api#12")
	if _, err := l1.New(pool).Delete(ctx, topic.OpenedBy); err != nil {
		t.Fatal(err)
	}
	if n := offered(retracted, public); n != 0 {
		t.Errorf("a topic whose document was retracted is still offered")
	}
}

// Issue #114: a position is shown to a document only where everyone who may
// read the document may read every piece of the position's evidence now.
func TestEvidenceIsReadableOnlyWhenEveryPieceIs(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	src := unique()
	group := connector.ACL{{Kind: connector.ACLGroup, Source: "github-acme", NativeID: "acme/api", Label: "collaborators"}}
	other := connector.ACL{{Kind: connector.ACLGroup, Source: "github-acme", NativeID: "acme/secret"}}
	open := putDoc(t, pool, src, "open", public, nil)
	shared := putDoc(t, pool, src, "shared", group, nil)
	secret := putDoc(t, pool, src, "secret", other, nil)
	resynced := putDoc(t, pool, src, "resynced", public, nil)
	putDoc(t, pool, src, "resynced", other, nil)

	tests := []struct {
		name     string
		evidence []string
		readers  connector.ACL
		want     bool
	}{
		{"one public document", []string{open}, public, true},
		{"public and the readers' own grant", []string{open, shared}, group, true},
		{"the same document twice", []string{shared, shared}, group, true},
		{"one piece under another grant", []string{open, secret}, group, false},
		{"a private piece for public readers", []string{open, shared}, public, false},
		{"a piece re-synced private", []string{open, resynced}, public, false},
		{"a piece that is not in L1", []string{open, "l1:" + src + ":gone"}, public, false},
		{"no evidence", nil, public, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.EvidenceReadableBy(ctx, tt.evidence, tt.readers)
			if err != nil || got != tt.want {
				t.Errorf("EvidenceReadableBy(%v) = %v, %v, want %v", tt.evidence, got, err, tt.want)
			}
		})
	}
}

// Issue #114: [l2.Access] decides from L1 as it is now, and fails closed on a
// document it cannot find.
func TestAccessIsTheEvidenceAsItIsNow(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	src := unique()
	kyleOnly := connector.ACL{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}}
	kyle := l1.Reader{
		Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.AllScopes()}},
		Audience:  []connector.ACLEntry{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}},
	}
	sam := l1.Reader{Effective: principal.Effective{Human: "sam", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
	open := putDoc(t, pool, src, "open", public, nil)
	private := putDoc(t, pool, src, "private", kyleOnly, nil)
	narrowed := putDoc(t, pool, src, "narrowed", public, nil)
	putDoc(t, pool, src, "narrowed", kyleOnly, nil)
	gone := "l1:" + src + ":gone"

	topic := func(opener string) l2.Topic { return l2.Topic{ID: "topic:" + opener, OpenedBy: opener, ACL: public} }
	stance := func(evidence ...string) l2.Stance { return l2.Stance{ID: "stance", Evidence: evidence, ACL: public} }
	topics := []l2.Topic{topic(open), topic(narrowed), topic(gone)}
	stances := []l2.Stance{stance(open, private), stance(narrowed), stance(gone)}
	access, err := store.Access(ctx, topics, stances)
	if err != nil {
		t.Fatalf("Access() = %v", err)
	}

	for _, tc := range []struct {
		name      string
		readable  func(l1.Reader) bool
		kyle, sam bool
	}{
		{"a topic opened by a public document", func(r l1.Reader) bool { return access.Topic(r, topics[0]) }, true, true},
		{"a topic whose document went private", func(r l1.Reader) bool { return access.Topic(r, topics[1]) }, true, false},
		{"a topic whose document is gone", func(r l1.Reader) bool { return access.Topic(r, topics[2]) }, false, false},
		{"a stance on public and private evidence", func(r l1.Reader) bool { return access.Stance(r, stances[0]) }, true, false},
		{"a stance whose evidence went private", func(r l1.Reader) bool { return access.Stance(r, stances[1]) }, true, false},
		{"a stance whose evidence is gone", func(r l1.Reader) bool { return access.Stance(r, stances[2]) }, false, false},
		{"a stance with no evidence", func(r l1.Reader) bool { return access.Stance(r, stance()) }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.readable(kyle); got != tc.kyle {
				t.Errorf("kyle may read = %v, want %v", got, tc.kyle)
			}
			if got := tc.readable(sam); got != tc.sam {
				t.Errorf("sam may read = %v, want %v", got, tc.sam)
			}
		})
	}
}

// embeddedDoc writes a public L1 document with a vector of its own.
func embeddedDoc(t *testing.T, pool *pgxpool.Pool, src, native string, vector []float32) string {
	t.Helper()
	return putDoc(t, pool, src, native, public, vector)
}

// putDoc writes an L1 document with this access list, and a vector where there
// is one. Writing it again with another list is what an ACL re-sync does.
func putDoc(t *testing.T, pool *pgxpool.Pool, src, native string, acl connector.ACL, vector []float32) string {
	t.Helper()
	when := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	doc := l1.Document{
		ID: l1.DocID(src, native), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: src, NativeID: native},
		L0Refs: []string{"evt:" + src + ":" + native}, Time: l1.Times{Created: when, Updated: when, LastActivity: when},
		ACL: acl, Text: "text of " + native, RawText: "raw " + native,
		Body: l1.Body{Summary: "s", OutcomeKind: l1.OutcomeDecided},
	}
	store := l1.New(pool)
	if _, err := store.Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	if vector != nil {
		if ok, err := store.SetEmbedding(t.Context(), doc.ID, doc.Text, vector); err != nil || !ok {
			t.Fatalf("SetEmbedding(%s) = %v, %v", doc.ID, ok, err)
		}
	}
	return doc.ID
}

// openTopicFrom opens a topic from an L1 document written with this access
// list, which is what decides who the topic is offered to.
func openTopicFrom(t *testing.T, pool *pgxpool.Pool, scope, native string, acl connector.ACL, keys ...string) l2.Topic {
	t.Helper()
	opener := putDoc(t, pool, scope, native, acl, nil)
	topic := l2.Topic{
		ID: l2.TopicID(scope, opener, 0, "the lock"), Scope: scope, Name: "the lock",
		JoinKeys: keys, ACL: acl, OpenedBy: opener,
	}
	if opened, err := l2.New(pool).OpenTopic(t.Context(), topic); err != nil || !opened {
		t.Fatalf("OpenTopic() = %v, %v", opened, err)
	}
	return topic
}

func unit(values ...float32) []float32 {
	v := make([]float32, l1.EmbeddingDimensions)
	copy(v, values)
	return v
}

func TestTopicsBySimilarityReadTheEvidenceVectors(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	src := unique()
	scope := src

	evidence := embeddedDoc(t, pool, src, "acme/api#1", unit(1, 0))
	near := embeddedDoc(t, pool, src, "acme/api#2", unit(0.99, 0.14))
	far := embeddedDoc(t, pool, src, "acme/api#3", unit(0, 1))
	bare := embeddedDoc(t, pool, src, "acme/api#4", nil)

	topic := openTopicFrom(t, pool, scope, "opener", public)
	if _, _, err := store.AppendStance(ctx, stance(topic, evidence, "a position", 1), time.Date(2026, 9, 9, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AppendStance() = %v", err)
	}

	tests := []struct {
		name    string
		doc     string
		exclude []string
		want    int
	}{
		{"a near document finds the topic", near, nil, 1},
		{"a far one does not", far, nil, 0},
		{"a document with no vector finds nothing", bare, nil, 0},
		{"a topic already found is not found twice", near, []string{topic.ID}, 0},
		{"the evidence itself does not count as near", evidence, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.TopicsBySimilarity(ctx, scope, tt.doc, public, 0.25, 5, tt.exclude)
			if err != nil {
				t.Fatalf("TopicsBySimilarity() = %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("TopicsBySimilarity() = %d topics, want %d", len(got), tt.want)
			}
		})
	}
}

// Every CHECK the store's validation mirrors holds on its own, for a row written
// by something other than this package.
func TestTheTablesRefuseWhatTheStoreRefuses(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	scope := unique()
	topicID := "topic:" + scope
	if _, err := pool.Exec(ctx, `INSERT INTO l2_topics (id, scope, name, acl, opened_by) VALUES ($1, $2, 'n', '[{"kind":"public"}]', 'd')`,
		topicID, scope); err != nil {
		t.Fatalf("inserting a valid topic: %v", err)
	}
	tests := []struct {
		name       string
		sql        string
		args       []any
		constraint string
	}{
		{"an unknown entity type", `INSERT INTO l2_entities (id, type, origin) VALUES ($1, 'widget', 'config')`,
			[]any{"e:" + scope}, "l2_entities_type_is_known"},
		{"an unknown origin", `INSERT INTO l2_entities (id, type, origin) VALUES ($1, 'module', 'guess')`,
			[]any{"e:" + scope}, "l2_entities_origin_is_known"},
		{"an empty entity id", `INSERT INTO l2_entities (id, type, origin) VALUES ('', 'module', 'config')`,
			nil, "l2_entities_id_is_not_empty"},
		{"a topic nobody may read", `INSERT INTO l2_topics (id, scope, name, acl, opened_by) VALUES ($1, 's', 'n', '[]', 'd')`,
			[]any{"t2:" + scope}, "l2_topics_acl_is_not_empty"},
		{"a topic with no name", `INSERT INTO l2_topics (id, scope, name, acl, opened_by) VALUES ($1, 's', '', '[{"kind":"public"}]', 'd')`,
			[]any{"t3:" + scope}, "l2_topics_name_fits"},
		{"an unknown tier", `INSERT INTO l2_stances (id, topic_id, position, stated_at, evidence, tier, acl)
			VALUES ($1, $2, 'p', now(), '{d}', 'certain', '[{"kind":"public"}]')`,
			[]any{"s1:" + scope, topicID}, "l2_stances_tier_is_known"},
		{"a position over the bound", `INSERT INTO l2_stances (id, topic_id, position, stated_at, evidence, tier, acl)
			VALUES ($1, $2, repeat('é', 1001), now(), '{d}', 'inferred', '[{"kind":"public"}]')`,
			[]any{"s2:" + scope, topicID}, "l2_stances_position_fits"},
		{"a stance with no evidence", `INSERT INTO l2_stances (id, topic_id, position, stated_at, evidence, tier, acl)
			VALUES ($1, $2, 'p', now(), '{}', 'inferred', '[{"kind":"public"}]')`,
			[]any{"s3:" + scope, topicID}, "l2_stances_has_evidence"},
		{"a stance nobody may read", `INSERT INTO l2_stances (id, topic_id, position, stated_at, evidence, tier, acl)
			VALUES ($1, $2, 'p', now(), '{d}', 'inferred', '[]')`,
			[]any{"s4:" + scope, topicID}, "l2_stances_acl_is_not_empty"},
		{"a stance superseding itself", `INSERT INTO l2_stances (id, topic_id, position, stated_at, evidence, supersedes, tier, acl)
			VALUES ($1, $2, 'p', now(), '{d}', $1, 'inferred', '[{"kind":"public"}]')`,
			[]any{"s5:" + scope, topicID}, "l2_stances_does_not_supersede_itself"},
		{"a negative stance count", `INSERT INTO l2_asserted (doc_id, distilled_at, stances) VALUES ($1, now(), -1)`,
			[]any{"l1:" + scope}, "l2_asserted_stances_is_not_negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tt.sql, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.constraint) {
				t.Errorf("insert = %v, want a violation of %s", err, tt.constraint)
			}
		})
	}
}

func TestPinsPersistInPinOrderAndUnpin(t *testing.T) {
	pool := newPool(t)
	scope := "code:" + unique()
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	a := l2.Pin{Scope: scope, L1: "l1:gh:acme/api#1", PinnedBy: "kyle", PinnedAt: at.Add(2 * time.Hour)}
	b := l2.Pin{Scope: scope, L1: "l1:gh:acme/api#2", PinnedBy: "sam", PinnedAt: at}
	c := l2.Pin{Scope: scope, L1: "l1:gh:acme/api#0", PinnedBy: "sam", PinnedAt: at}
	for _, p := range []l2.Pin{a, b, c} {
		if ok, err := l2.New(pool).Pin(t.Context(), p); err != nil || !ok {
			t.Fatalf("Pin(%s) = %v, %v; want it recorded", p.L1, ok, err)
		}
	}
	// Pinning again changes nothing: the first pinner and time stay.
	if ok, err := l2.New(pool).Pin(t.Context(), l2.Pin{Scope: scope, L1: a.L1, PinnedBy: "sam", PinnedAt: at.Add(-time.Hour)}); err != nil || ok {
		t.Fatalf("pinning %s again = %v, %v; want nothing written", a.L1, ok, err)
	}
	// Another scope's pin is not this one's.
	if _, err := l2.New(pool).Pin(t.Context(), l2.Pin{Scope: scope + "/other", L1: a.L1, PinnedBy: "kyle", PinnedAt: at}); err != nil {
		t.Fatal(err)
	}

	got, err := l2.New(pool).Pins(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if want := []l2.Pin{c, b, a}; !slices.Equal(got, want) {
		t.Errorf("pins = %+v, want first pinned first, a tie in id order: %+v", got, want)
	}

	for _, tt := range []struct {
		name    string
		doc     string
		removed bool
		left    []l2.Pin
	}{
		{"unpinning removes the pin", b.L1, true, []l2.Pin{c, a}},
		{"unpinning what is not pinned removes nothing", b.L1, false, []l2.Pin{c, a}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			removed, err := l2.New(pool).Unpin(t.Context(), scope, tt.doc)
			if err != nil || removed != tt.removed {
				t.Fatalf("Unpin(%s) = %v, %v; want %v", tt.doc, removed, err, tt.removed)
			}
			left, err := l2.New(pool).Pins(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(left, tt.left) {
				t.Errorf("pins = %+v, want %+v", left, tt.left)
			}
		})
	}

	for _, p := range []l2.Pin{
		{L1: a.L1, PinnedBy: "kyle", PinnedAt: at},
		{Scope: scope, L1: "acme/api#1", PinnedBy: "kyle", PinnedAt: at},
		{Scope: scope, L1: a.L1, PinnedAt: at},
		{Scope: scope, L1: a.L1, PinnedBy: "kyle"},
	} {
		if _, err := l2.New(pool).Pin(t.Context(), p); !errors.Is(err, l2.ErrInvalid) {
			t.Errorf("Pin(%+v) = %v, want ErrInvalid", p, err)
		}
	}
}

func TestAliasDecisionAndRejectedVote(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	ctx := t.Context()
	src := unique()
	entity := "code:" + src + ":engine"
	if err := store.PutEntity(ctx, l2.Entity{ID: entity, Type: l2.TypeModule, Name: src + " core", Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	doc := putDoc(t, pool, src, "thread", public, nil)
	pr := putDoc(t, pool, src, "pr", public, nil)
	if err := store.VoteAlias(ctx, entity, src+" Room", doc, pr, public, public); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Resolve(ctx, src+" room"); err != nil || len(got) != 0 {
		t.Fatalf("proposed resolve = %+v, %v", got, err)
	}
	if err := store.SetAliasState(ctx, entity, src+" room", "confirmed"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Resolve(ctx, src+" room"); err != nil || len(got) != 1 || got[0].Entity.ID != entity {
		t.Fatalf("confirmed resolve = %+v, %v", got, err)
	}
	other := "code:" + src + ":other"
	if err := store.PutEntity(ctx, l2.Entity{ID: other, Type: l2.TypeModule, Name: src + " other", Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	if err := store.VoteAlias(ctx, other, src+" Room", doc, pr, public, public); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAliasState(ctx, other, src+" room", "confirmed"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Resolve(ctx, src+" room"); err != nil || len(got) != 0 {
		t.Fatalf("ambiguous resolve = %+v, %v", got, err)
	}
	rejected := "code:" + src + ":rejected"
	if err := store.PutEntity(ctx, l2.Entity{ID: rejected, Type: l2.TypeModule, Name: src + " rejected", Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	if err := store.VoteAlias(ctx, rejected, src+" Old Name", doc, pr, public, public); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAliasState(ctx, rejected, src+" old name", "rejected"); err != nil {
		t.Fatal(err)
	}
	later := putDoc(t, pool, src, "later", public, nil)
	if err := store.VoteAlias(ctx, rejected, src+" Old Name", later, pr, public, public); err != nil {
		t.Fatal(err)
	}
	c, err := store.AliasCandidate(ctx, rejected, src+" old name")
	if err != nil || c.State != "rejected" || c.Votes != 1 {
		t.Fatalf("rejected candidate = %+v, %v", c, err)
	}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u1"}}
	secretDoc := putDoc(t, pool, src, "secret", private, nil)
	if err := store.VoteAlias(ctx, entity, src+" secret", secretDoc, pr, private, public); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAliasState(ctx, entity, src+" secret", "confirmed"); err != nil {
		t.Fatal(err)
	}
	outsider := l1.Reader{Effective: principal.Effective{Human: "outsider"}}
	if got, err := store.ResolveFor(ctx, outsider, src+" secret"); err != nil || len(got) != 0 {
		t.Fatalf("outsider resolve = %+v, %v", got, err)
	}
	insider := l1.Reader{Effective: principal.Effective{Human: "insider"}, Audience: private}
	if got, err := store.ResolveFor(ctx, insider, src+" secret"); err != nil || len(got) != 1 || got[0].Entity.ID != entity {
		t.Fatalf("insider resolve = %+v, %v", got, err)
	}
}

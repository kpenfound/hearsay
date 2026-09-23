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
	if err := store.ReplaceSeeded(ctx, []l2.Entity{item}); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("ReplaceSeeded(a reference) = %v, want ErrInvalid", err)
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
	second, _ := add(stance(topic, "l1:s:pr", "engine takes the lock", 5))
	if second.Supersedes != first.ID {
		t.Errorf("the second stance supersedes %q, want %q", second.Supersedes, first.ID)
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
	openTopic(t, store, scope, connector.ACL{group}, "item:acme/api#12")

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
	more := l2.Topic{ID: "topic:" + unique(), Scope: scope, Name: "two keys",
		JoinKeys: []string{"item:acme/api#12", "item:acme/api#31"}, ACL: public, OpenedBy: "l1:s:x"}
	if _, err := store.OpenTopic(ctx, more); err != nil {
		t.Fatalf("OpenTopic() = %v", err)
	}
	got, err := store.TopicsByJoinKeys(ctx, scope, []string{"item:acme/api#12", "item:acme/api#31"}, connector.ACL{group}, 5)
	if err != nil || len(got) != 2 || got[0].ID != more.ID {
		t.Errorf("TopicsByJoinKeys(two keys) = %+v, %v, want both, the one sharing two keys first", got, err)
	}
}

// embeddedDoc writes an L1 document with a vector of its own.
func embeddedDoc(t *testing.T, pool *pgxpool.Pool, src, native string, vector []float32) string {
	t.Helper()
	when := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	doc := l1.Document{
		ID: l1.DocID(src, native), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: src, NativeID: native},
		L0Refs: []string{"evt:" + src + ":" + native}, Time: l1.Times{Created: when, Updated: when, LastActivity: when},
		ACL: public, Text: "text of " + native, RawText: "raw " + native,
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

	topic := openTopic(t, store, scope, public)
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

//go:build integration

package l3_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
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

var sources atomic.Int64

// newSource is a source id, and so an entity namespace, nothing else uses: the
// database outlives one test.
func newSource() string {
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(sources.Add(1), 36)
}

var (
	day     = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	public  = connector.ACL{{Kind: connector.ACLPublic}}
	private = connector.ACL{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}}
	// kyle may read the private list; sam may read only what is public.
	kyle = l1.Reader{
		Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.AllScopes()}},
		Audience:  []connector.ACLEntry{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}},
	}
	sam = l1.Reader{Effective: principal.Effective{Human: "sam", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
)

func putDoc(t *testing.T, pool *pgxpool.Pool, src, artifact string, kind l1.Kind, hour int, scope string, acl connector.ACL, questions ...string) string {
	t.Helper()
	at := day.Add(time.Duration(hour) * time.Hour)
	doc := l1.Document{
		ID: l1.DocID(src, artifact), Kind: kind, ArtifactClass: config.ArtifactIssue, Source: l1.Source{System: src, NativeID: artifact},
		L0Refs: []string{connector.EventID(src, artifact)}, Time: l1.Times{Created: at, Updated: at, LastActivity: at},
		Scope: []string{scope}, ACL: acl, Text: artifact, RawText: artifact,
		Body: l1.Body{Summary: artifact, OutcomeKind: l1.OutcomeNone, OpenQuestions: questions},
	}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	return doc.ID
}

func ids(docs []l1.Stored) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}
	return out
}

func TestRecentDedupesByKindAndFiltersBeforeItCounts(t *testing.T) {
	pool := newPool(t)
	views := l3.New(pool)
	src := newSource()
	scope := "tracker:" + src + ":acme/api#1"

	issue := putDoc(t, pool, src, "acme/api#1", l1.KindIssue, 1, scope, public)
	var commits []string
	for i := range 6 {
		commits = append(commits, putDoc(t, pool, src, fmt.Sprintf("c%d", i), l1.KindCommit, 10+i, scope, public))
	}
	got, err := views.Recent(t.Context(), sam, scope, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{commits[5], commits[4], commits[3], commits[2], issue}
	if !slices.Equal(ids(got.Items), want) {
		t.Errorf("recent = %v, want the four newest commits and the one issue, newest first: %v", ids(got.Items), want)
	}
	if !got.LastActivity.Equal(day.Add(15 * time.Hour)) {
		t.Errorf("last_activity = %s, want the newest commit's", got.LastActivity)
	}

	// Six documents sam may not read, all newer than anything sam may: none of
	// them takes a place, and none of them is sam's last activity.
	hiddenScope := "tracker:" + src + ":acme/api#2"
	visible := putDoc(t, pool, src, "acme/api#2", l1.KindIssue, 1, hiddenScope, public)
	for i := range 6 {
		putDoc(t, pool, src, fmt.Sprintf("p%d", i), l1.KindIssue, 20+i, hiddenScope, private)
	}
	got, err = views.Recent(t.Context(), sam, hiddenScope, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.Items), []string{visible}) || !got.LastActivity.Equal(day.Add(time.Hour)) {
		t.Errorf("sam's recent = %v at %s, want only %s at its own time", ids(got.Items), got.LastActivity, visible)
	}
	withheld, err := l1.New(pool).Withheld(t.Context(), sam, hiddenScope)
	if err != nil || withheld != 6 {
		t.Errorf("Withheld(sam) = %d, %v, want 6", withheld, err)
	}
	if withheld, err := l1.New(pool).Withheld(t.Context(), kyle, hiddenScope); err != nil || withheld != 0 {
		t.Errorf("Withheld(kyle) = %d, %v, want 0", withheld, err)
	}
}

func TestOpenQuestionsReadOnlyDocumentsThatLeaveSomethingOpen(t *testing.T) {
	pool := newPool(t)
	src := newSource()
	scope := "tracker:" + src + ":acme/api#1"
	// The same question twice in one document is one piece of evidence.
	first := putDoc(t, pool, src, "q1", l1.KindIssue, 1, scope, public, "who runs it?", "when?", " who runs it? ")
	second := putDoc(t, pool, src, "q2", l1.KindIssue, 2, scope, public, "who runs it?")
	putDoc(t, pool, src, "q0", l1.KindIssue, 0, scope, public, "an older question")
	for i := range 3 {
		putDoc(t, pool, src, fmt.Sprintf("n%d", i), l1.KindIssue, 10+i, scope, public)
	}
	got, err := l3.New(pool).OpenQuestions(t.Context(), sam, scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []l3.Question{{Text: "who runs it?", Evidence: []string{second, first}}, {Text: "when?", Evidence: []string{first}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("open questions = %v, want %v", got, want)
	}
	// A listing with no reader does not filter on open questions, and says so
	// rather than returning documents that have none.
	if _, err := l1.New(pool).List(t.Context(), l1.ListOptions{Scope: scope, OpenQuestions: true}); !errors.Is(err, l1.ErrInvalidDocument) {
		t.Errorf("List(OpenQuestions) = %v, want ErrInvalidDocument", err)
	}
}

func TestCurrentStancesAreTheHeadsTheReaderMayRead(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	graph := l2.New(pool)
	src := newSource()
	child, parent := "code:"+src+":child", "code:"+src+":parent"
	// A cycle: the walk up part_of must end anyway.
	for _, e := range []l2.Entity{
		{ID: child, Type: l2.TypeModule, PartOf: []string{parent}, Origin: l2.OriginConfig},
		{ID: parent, Type: l2.TypeProject, PartOf: []string{child}, Origin: l2.OriginConfig},
	} {
		if err := graph.PutEntity(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// Who may read a topic or a stance is its documents' access lists, so
	// every document is written with the list the case needs.
	opener := putDoc(t, pool, src, "x", l1.KindIssue, 0, child, public)
	topic := func(name string, about string) l2.Topic {
		t.Helper()
		tp := l2.Topic{ID: l2.TopicID(src, opener, 0, name), Scope: src, Name: name, About: []string{about}, ACL: public, OpenedBy: opener}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		return tp
	}
	doc := func(artifact string, acl connector.ACL) string {
		t.Helper()
		return putDoc(t, pool, src, artifact, l1.KindIssue, 0, child, acl)
	}
	stance := func(tp l2.Topic, doc, position string, hour int) {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		_, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: public,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	forked := topic("forked", child)
	stance(forked, doc("a", public), "first", 1)
	stance(forked, doc("b", public), "newest", 5)
	stance(forked, doc("c", public), "read late", 3) // forks behind the head
	// A document read again, stated earlier than it was the first time (a
	// deletion moved its last activity back): the newer stance it retired is
	// not current.
	restated := topic("restated", child)
	k := doc("k", public)
	stance(restated, k, "said at first", 8)
	stance(restated, k, "said again", 6)
	secretPast := topic("a private past", child)
	stance(secretPast, doc("d", private), "what kyle alone saw", 1)
	stance(secretPast, doc("e", public), "what everyone sees", 2)
	secretNow := topic("a private present", child)
	stance(secretNow, doc("f", public), "public once", 1)
	stance(secretNow, doc("g", private), "private now", 3)
	// Newer than every stance of the child's own, so only the sort puts them
	// last; the related entity's topic is inherited though no ancestor walk
	// reached it.
	sibling := "code:" + src + ":sibling"
	stance(topic("inherited", parent), doc("h", public), "from the parent", 9)
	stance(topic("via a related entity", sibling), doc("j", public), "from the sibling", 8)
	// A topic sam may not read with a stance sam may: the store allows it, and
	// the topic's name is what must not reach sam.
	privateOpener := doc("y", private)
	privateTopic := l2.Topic{ID: l2.TopicID(src, privateOpener, 0, "a private topic"), Scope: src, Name: "a private topic",
		About: []string{child}, ACL: private, OpenedBy: privateOpener}
	if _, err := graph.OpenTopic(ctx, privateTopic); err != nil {
		t.Fatal(err)
	}
	stance(privateTopic, doc("i", public), "a public position", 4)

	type row struct {
		Topic, Current, Supersedes string
		Inherited                  bool
	}
	read := func(reader l1.Reader) ([]row, int) {
		t.Helper()
		got, withheld, err := l3.New(pool).CurrentStances(ctx, reader, child, []string{sibling})
		if err != nil {
			t.Fatal(err)
		}
		var rows []row
		for _, c := range got {
			rows = append(rows, row{c.Topic.Name, c.Stance.Position, c.Supersedes, c.Inherited})
		}
		return rows, withheld
	}

	sams, withheld := read(sam)
	wantSam := []row{
		{"restated", "said again", "said at first", false},
		{"forked", "newest", "first", false},
		{"a private past", "what everyone sees", "", false},
		{"inherited", "from the parent", "", true},
		{"via a related entity", "from the sibling", "", true},
	}
	if fmt.Sprint(sams) != fmt.Sprint(wantSam) || withheld != 2 {
		t.Errorf("sam's stances = %v with %d withheld, want %v with 2", sams, withheld, wantSam)
	}
	kyles, withheld := read(kyle)
	wantKyle := []row{
		{"restated", "said again", "said at first", false},
		{"forked", "newest", "first", false},
		{"a private topic", "a public position", "", false},
		{"a private present", "private now", "public once", false},
		{"a private past", "what everyone sees", "what kyle alone saw", false},
		{"inherited", "from the parent", "", true},
		{"via a related entity", "from the sibling", "", true},
	}
	if fmt.Sprint(kyles) != fmt.Sprint(wantKyle) || withheld != 0 {
		t.Errorf("kyle's stances = %v with %d withheld, want %v with 0", kyles, withheld, wantKyle)
	}
}

// Issue #114: a topic and its stance are read on what their documents allow
// now. The access lists written on them say everyone may read both, and none of
// them is what decides.
func TestStancesFollowEveryPieceOfTheirEvidenceNow(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	graph := l2.New(pool)
	docs := l1.New(pool)
	src := newSource()
	entity := "code:" + src + ":api"
	if err := graph.PutEntity(ctx, l2.Entity{ID: entity, Type: l2.TypeModule, Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	doc := func(artifact string, acl connector.ACL) string {
		t.Helper()
		return putDoc(t, pool, src, artifact, l1.KindIssue, 0, entity, acl)
	}
	stance := func(tp l2.Topic, position string, hour int, evidence ...string) {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		if _, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, evidence[0], position, at, l2.TierInferred), TopicID: tp.ID, Position: position,
			StatedAt: at, Evidence: evidence, Tier: l2.TierInferred, ACL: public,
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	topic := func(name, opener string) l2.Topic {
		t.Helper()
		tp := l2.Topic{ID: l2.TopicID(src, opener, 0, name), Scope: src, Name: name, About: []string{entity}, ACL: public, OpenedBy: opener}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		return tp
	}

	// Two pieces of evidence, one of them only kyle's.
	both := doc("both", public)
	stance(topic("partial evidence", both), "rests on two", 1, both, doc("kyles half", private))
	// A topic whose opening document goes private after it was opened.
	narrowed := doc("narrowed", public)
	stance(topic("opener narrowed", narrowed), "on an open stance", 1, doc("open evidence", public))
	// A stance whose evidence goes private after it was read, over a stance
	// that stays public: sam is not offered the older one in its place.
	head, prior := doc("head", public), doc("prior", public)
	evidenceNarrowed := topic("evidence narrowed", prior)
	stance(evidenceNarrowed, "the older position", 1, prior)
	stance(evidenceNarrowed, "the newer position", 2, head)
	// A stance whose evidence is retracted, and one whose evidence never was in L1.
	retracted := doc("retracted", public)
	stance(topic("evidence retracted", retracted), "retracted evidence", 1, retracted)
	missing := doc("missing opener", public)
	stance(topic("evidence missing", missing), "rests on nothing", 1, "l1:"+src+":never")
	// A topic whose opening document is retracted.
	gone := doc("gone", public)
	stance(topic("opener retracted", gone), "on a gone topic", 1, doc("still here", public))
	// The one that stays readable, superseding a stance whose evidence goes
	// private: it stays, and no longer names what it superseded.
	stays := doc("stays", public)
	kept := topic("kept", stays)
	stance(kept, "what went private", 1, doc("went private", public))
	stance(kept, "what everyone reads", 2, stays)

	for _, id := range []string{narrowed, head, l1.DocID(src, "went private")} {
		putDoc(t, pool, src, strings.TrimPrefix(id, "l1:"+src+":"), l1.KindIssue, 0, entity, private)
	}
	for _, id := range []string{retracted, gone} {
		if _, err := docs.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	read := func(reader l1.Reader) (map[string]string, int) {
		t.Helper()
		got, withheld, err := l3.New(pool).CurrentStances(ctx, reader, entity, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, c := range got {
			out[c.Topic.Name] = c.Stance.Position + " / " + c.Supersedes
		}
		return out, withheld
	}
	sams, withheld := read(sam)
	if want := map[string]string{"kept": "what everyone reads / "}; fmt.Sprint(sams) != fmt.Sprint(want) || withheld != 6 {
		t.Errorf("sam's stances = %v with %d withheld, want %v with 6", sams, withheld, want)
	}
	kyles, withheld := read(kyle)
	wantKyle := map[string]string{
		"partial evidence":  "rests on two / ",
		"opener narrowed":   "on an open stance / ",
		"evidence narrowed": "the newer position / the older position",
		"kept":              "what everyone reads / what went private",
	}
	if fmt.Sprint(kyles) != fmt.Sprint(wantKyle) || withheld != 3 {
		t.Errorf("kyle's stances = %v with %d withheld, want %v with 3", kyles, withheld, wantKyle)
	}

	// The same through the bundle: nothing of what sam may not read is in it.
	b, report, err := bundle.New(pool).Assemble(ctx, sam, entity)
	if err != nil {
		t.Fatal(err)
	}
	body, err := bundle.Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"rests on two", "partial evidence", "opener narrowed", "the newer position", "the older position",
		"retracted evidence", "rests on nothing", "on a gone topic", "what went private"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("sam's bundle holds %q: %s", secret, body)
		}
	}
	if report.Withheld.Stances != 6 {
		t.Errorf("sam's bundle withheld %d stances, want 6", report.Withheld.Stances)
	}
}

func TestTheSubjectIsTheDocumentThatSaysItIsTheEntity(t *testing.T) {
	pool := newPool(t)
	views := l3.New(pool)
	src := newSource()
	entity := "tracker:" + src + ":acme/api#1"
	putDoc(t, pool, src, "acme/api#1", l1.KindIssue, 1, entity, private)
	putDoc(t, pool, src, "acme/api#2", l1.KindIssue, 1, "tracker:"+src+":elsewhere#2", public)

	for _, tc := range []struct {
		name   string
		reader l1.Reader
		entity string
		want   bool
	}{
		{"a reader who may read it", kyle, entity, true},
		{"a reader who may not", sam, entity, false},
		{"a document that is not about the entity", kyle, "tracker:" + src + ":acme/api#2", false},
		{"no such document", kyle, "tracker:" + src + ":acme/api#3", false},
		{"not a tracker item", kyle, "code:" + src, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := views.Subject(t.Context(), tc.reader, tc.entity)
			if err != nil || ok != tc.want {
				t.Errorf("Subject(%s) = %v, %v, want %v", tc.entity, ok, err, tc.want)
			}
		})
	}
}

// A scope the reader was not granted assembles to a bundle with nothing in it,
// even where the graph holds stances about it.
func TestABundleForAScopeNotGrantedIsEmpty(t *testing.T) {
	pool := newPool(t)
	src := newSource()
	scope := "tracker:" + src + ":acme/api#1"
	issue := putDoc(t, pool, src, "acme/api#1", l1.KindIssue, 1, scope, public, "open?")
	graph := l2.New(pool)
	tp := l2.Topic{ID: l2.TopicID(src, issue, 0, "t"), Scope: src, Name: "t", About: []string{scope}, ACL: public, OpenedBy: issue}
	if _, err := graph.OpenTopic(t.Context(), tp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(tp.ID, issue, "p", day, l2.TierRatified), TopicID: tp.ID, Position: "p",
		StatedAt: day, Evidence: []string{issue}, Tier: l2.TierRatified, ACL: public}, day); err != nil {
		t.Fatal(err)
	}

	elsewhere := sam
	elsewhere.Effective.Grant.Scopes = principal.SomeScopes("tracker:" + src + ":acme/api#9")
	for _, tc := range []struct {
		name   string
		reader l1.Reader
		empty  bool
	}{
		{"granted", sam, false},
		{"not granted", elsewhere, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, err := bundle.New(pool).Assemble(t.Context(), tc.reader, scope)
			if err != nil {
				t.Fatal(err)
			}
			empty := len(b.Scope.Entities) == 0 && len(b.Stances) == 0 && len(b.Recent.Items) == 0 && len(b.OpenQuestions) == 0
			if empty != tc.empty {
				t.Errorf("bundle = %+v, want empty=%v", b, tc.empty)
			}
		})
	}
}

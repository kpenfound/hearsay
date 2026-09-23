//go:build integration

package l3_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
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
	got, err := views.Recent(t.Context(), sam, scope, 5, nil)
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
	got, err = views.Recent(t.Context(), sam, hiddenScope, 1, nil)
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
	topic := func(name string, about string) l2.Topic {
		t.Helper()
		tp := l2.Topic{ID: l2.TopicID(src, "l1:x", 0, name), Scope: src, Name: name, About: []string{about}, ACL: public, OpenedBy: "l1:x"}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		return tp
	}
	stance := func(tp l2.Topic, doc, position string, hour int, acl connector.ACL) {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		_, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: acl,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	forked := topic("forked", child)
	stance(forked, "l1:a", "first", 1, public)
	stance(forked, "l1:b", "newest", 5, public)
	stance(forked, "l1:c", "read late", 3, public) // forks behind the head
	// A document read again, stated earlier than it was the first time (a
	// deletion moved its last activity back): the newer stance it retired is
	// not current.
	restated := topic("restated", child)
	stance(restated, "l1:k", "said at first", 8, public)
	stance(restated, "l1:k", "said again", 6, public)
	secretPast := topic("a private past", child)
	stance(secretPast, "l1:d", "what kyle alone saw", 1, private)
	stance(secretPast, "l1:e", "what everyone sees", 2, public)
	secretNow := topic("a private present", child)
	stance(secretNow, "l1:f", "public once", 1, public)
	stance(secretNow, "l1:g", "private now", 3, private)
	// Newer than every stance of the child's own, so only the sort puts them
	// last; the related entity's topic is inherited though no ancestor walk
	// reached it.
	sibling := "code:" + src + ":sibling"
	stance(topic("inherited", parent), "l1:h", "from the parent", 9, public)
	stance(topic("via a related entity", sibling), "l1:j", "from the sibling", 8, public)
	// A topic sam may not read with a stance sam may: the store allows it, and
	// the topic's name is what must not reach sam.
	privateTopic := l2.Topic{ID: l2.TopicID(src, "l1:x", 0, "a private topic"), Scope: src, Name: "a private topic",
		About: []string{child}, ACL: private, OpenedBy: "l1:x"}
	if _, err := graph.OpenTopic(ctx, privateTopic); err != nil {
		t.Fatal(err)
	}
	stance(privateTopic, "l1:i", "a public position", 4, public)

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
	putDoc(t, pool, src, "acme/api#1", l1.KindIssue, 1, scope, public, "open?")
	graph := l2.New(pool)
	tp := l2.Topic{ID: l2.TopicID(src, "l1:x", 0, "t"), Scope: src, Name: "t", About: []string{scope}, ACL: public, OpenedBy: "l1:x"}
	if _, err := graph.OpenTopic(t.Context(), tp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(tp.ID, "l1:x", "p", day, l2.TierRatified), TopicID: tp.ID, Position: "p",
		StatedAt: day, Evidence: []string{"l1:x"}, Tier: l2.TierRatified, ACL: public}, day); err != nil {
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

// anchorDoc is a document about scope of an artifact class, pointing at refs,
// with a permalink a `url` reference can name.
func anchorDoc(t *testing.T, pool *pgxpool.Pool, src, artifact string, kind l1.Kind, class config.ArtifactClass, hour int, scope string, acl connector.ACL, refs ...l1.Reference) string {
	t.Helper()
	at := day.Add(time.Duration(hour) * time.Hour)
	doc := l1.Document{
		ID: l1.DocID(src, artifact), Kind: kind, ArtifactClass: class,
		Source: l1.Source{System: src, NativeID: artifact, URL: "https://wiki.example/" + src + "/" + artifact},
		L0Refs: []string{connector.EventID(src, artifact)}, Time: l1.Times{Created: at, Updated: at, LastActivity: at},
		Scope: []string{scope}, References: refs, ACL: acl, Text: artifact, RawText: artifact,
		Body: l1.Body{Summary: artifact, OutcomeKind: l1.OutcomeNone},
	}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	return doc.ID
}

func pin(t *testing.T, pool *pgxpool.Pool, scope, doc, by string, hour int) {
	t.Helper()
	if _, err := l2.New(pool).Pin(t.Context(), l2.Pin{Scope: scope, L1: doc, PinnedBy: by, PinnedAt: day.Add(time.Duration(hour) * time.Hour)}); err != nil {
		t.Fatalf("Pin(%s) = %v", doc, err)
	}
}

// anchored is the anchors as `id` or `id@pinner`.
func anchored(anchors []l3.Anchor) []string {
	out := make([]string, len(anchors))
	for i, a := range anchors {
		out[i] = a.Doc.ID
		if a.PinnedBy != "" {
			out[i] += "@" + a.PinnedBy
		}
	}
	return out
}

func item(id string) l1.Reference    { return l1.Reference{Type: l1.RefIssue, ID: id} }
func tracker(id string) l1.Reference { return l1.Reference{Type: l1.RefTrackerItem, ID: id} }
func link(url string) l1.Reference   { return l1.Reference{Type: l1.RefURL, ID: url} }

func TestAnchorsArePinnedThenInferredThenDefaulted(t *testing.T) {
	pool := newPool(t)
	views := l3.New(pool)

	tests := []struct {
		name string
		// build writes a scope's graph and returns the anchors each reader
		// should get, as [anchored] spells them.
		build func(t *testing.T, src, scope string) (kyles, sams []string)
	}{
		{"pinned first pinned first, then the most referenced, then specs newest first, capped at four",
			func(t *testing.T, src, scope string) ([]string, []string) {
				design := anchorDoc(t, pool, src, "acme/api#10", l1.KindIssue, config.ArtifactIssue, 1, scope, public)
				anchorDoc(t, pool, src, "acme/api#11", l1.KindIssue, config.ArtifactIssue, 2, scope, public)
				// #10 is pointed at by three documents, however they spell it;
				// #11 by two.
				anchorDoc(t, pool, src, "c1", l1.KindCommit, config.ArtifactCommit, 3, scope, public, item("acme/api#10"))
				anchorDoc(t, pool, src, "c2", l1.KindCommit, config.ArtifactCommit, 4, scope, public, item("acme/api#11"), tracker("acme/api#10"))
				anchorDoc(t, pool, src, "c3", l1.KindCommit, config.ArtifactCommit, 5, scope, public, item("acme/api#11"), l1.Reference{Type: l1.RefPR, ID: "acme/api#10"})
				anchorDoc(t, pool, src, "spec1.md", l1.KindWikiSection, config.ArtifactSpec, 6, scope, public)
				anchorDoc(t, pool, src, "spec2.md", l1.KindWikiSection, config.ArtifactSpec, 7, scope, public)
				spec3 := anchorDoc(t, pool, src, "spec3.md", l1.KindWikiSection, config.ArtifactSpec, 8, scope, public)
				a := anchorDoc(t, pool, src, "acme/api#20", l1.KindIssue, config.ArtifactIssue, 9, scope, public)
				b := anchorDoc(t, pool, src, "acme/api#21", l1.KindIssue, config.ArtifactIssue, 10, scope, public)
				// Pinned second, but earlier: pins are ordered by when.
				pin(t, pool, scope, a, "kyle", 2)
				pin(t, pool, scope, b, "sam", 1)
				want := []string{b + "@sam", a + "@kyle", design, spec3}
				return want, want
			}},
		{"a document is an anchor once, where it first qualifies, and a link names a document by its permalink",
			func(t *testing.T, src, scope string) ([]string, []string) {
				spec1 := anchorDoc(t, pool, src, "spec1.md", l1.KindWikiSection, config.ArtifactSpec, 1, scope, public)
				spec2 := anchorDoc(t, pool, src, "spec2.md", l1.KindWikiSection, config.ArtifactSpec, 2, scope, public)
				spec3 := anchorDoc(t, pool, src, "spec3.md", l1.KindWikiSection, config.ArtifactSpec, 3, scope, public)
				url := "https://wiki.example/" + src + "/spec1.md"
				anchorDoc(t, pool, src, "chat1", l1.KindChatThread, config.ArtifactChatThread, 4, scope, public, link(url))
				anchorDoc(t, pool, src, "chat2", l1.KindChatThread, config.ArtifactChatThread, 5, scope, public, link(url))
				// Pinned, most referenced and a spec: pinned, once.
				pin(t, pool, scope, spec1, "kyle", 1)
				want := []string{spec1 + "@kyle", spec3, spec2}
				return want, want
			}},
		{"a pin the reader may not read, or that names no document, takes no place",
			func(t *testing.T, src, scope string) ([]string, []string) {
				hidden := anchorDoc(t, pool, src, "acme/api#1", l1.KindIssue, config.ArtifactIssue, 1, scope, private)
				var shown []string
				for i := range 3 {
					shown = append(shown, anchorDoc(t, pool, src, fmt.Sprintf("acme/api#%d", 2+i), l1.KindIssue, config.ArtifactIssue, 2+i, scope, public))
				}
				spec := anchorDoc(t, pool, src, "spec.md", l1.KindWikiSection, config.ArtifactSpec, 9, scope, public)
				pin(t, pool, scope, l1.DocID(src, "gone"), "kyle", 0)
				pin(t, pool, scope, hidden, "kyle", 1)
				for i, id := range shown {
					pin(t, pool, scope, id, "kyle", 2+i)
				}
				kyles := []string{hidden + "@kyle", shown[0] + "@kyle", shown[1] + "@kyle", shown[2] + "@kyle"}
				sams := []string{shown[0] + "@kyle", shown[1] + "@kyle", shown[2] + "@kyle", spec}
				return kyles, sams
			}},
		{"inference counts only what the reader may read, on either end",
			func(t *testing.T, src, scope string) ([]string, []string) {
				secret := anchorDoc(t, pool, src, "acme/api#1", l1.KindIssue, config.ArtifactIssue, 1, scope, private)
				open := anchorDoc(t, pool, src, "acme/api#2", l1.KindIssue, config.ArtifactIssue, 2, scope, public)
				anchorDoc(t, pool, src, "acme/api#3", l1.KindIssue, config.ArtifactIssue, 3, scope, public)
				// Three public documents point at the private #1, two private
				// ones at #3, and one public one at #2: sam may read neither #1
				// nor what points at #3.
				for i := range 3 {
					anchorDoc(t, pool, src, fmt.Sprintf("q%d", i), l1.KindChatThread, config.ArtifactChatThread, 4+i, scope, public, item("acme/api#1"))
				}
				anchorDoc(t, pool, src, "p1", l1.KindChatThread, config.ArtifactChatThread, 7, scope, private, item("acme/api#3"))
				anchorDoc(t, pool, src, "p2", l1.KindChatThread, config.ArtifactChatThread, 8, scope, private, item("acme/api#3"))
				anchorDoc(t, pool, src, "q9", l1.KindChatThread, config.ArtifactChatThread, 9, scope, public, item("acme/api#2"))
				return []string{secret}, []string{open}
			}},
		{"a document nothing points at is not inferred, and a scope with nothing has no anchors",
			func(t *testing.T, src, scope string) ([]string, []string) {
				anchorDoc(t, pool, src, "acme/api#1", l1.KindIssue, config.ArtifactIssue, 1, scope, public)
				anchorDoc(t, pool, src, "acme/api#2", l1.KindIssue, config.ArtifactIssue, 2, scope, public, item("acme/api#2"))
				return []string{}, []string{}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource()
			scope := "code:" + src
			kyles, sams := tt.build(t, src, scope)
			for _, r := range []struct {
				name   string
				reader l1.Reader
				want   []string
			}{{"kyle", kyle, kyles}, {"sam", sam, sams}} {
				got, err := views.Anchors(t.Context(), r.reader, scope, bundle.AnchorCap)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(anchored(got), r.want) {
					t.Errorf("%s's anchors = %v, want %v", r.name, anchored(got), r.want)
				}
				again, err := views.Anchors(t.Context(), r.reader, scope, bundle.AnchorCap)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(anchored(again), anchored(got)) {
					t.Errorf("%s's anchors read twice = %v then %v", r.name, anchored(got), anchored(again))
				}
			}
		})
	}
}

// An anchor is not activity: it is not repeated in `recent`, and it does not
// cost `recent` a place.
func TestAnAnchorIsNotRepeatedInRecent(t *testing.T) {
	pool := newPool(t)
	src := newSource()
	scope := "code:" + src
	var issues []string
	for i := range 6 {
		issues = append(issues, anchorDoc(t, pool, src, fmt.Sprintf("acme/api#%d", i), l1.KindIssue, config.ArtifactIssue, i, scope, public))
	}
	// The newest document in the scope is the spec it defaults to, and the
	// newest issue is pinned.
	spec := anchorDoc(t, pool, src, "spec.md", l1.KindWikiSection, config.ArtifactSpec, 20, scope, public)
	pin(t, pool, scope, issues[5], "kyle", 0)

	b, _, err := bundle.New(pool).Assemble(t.Context(), sam, scope)
	if err != nil {
		t.Fatal(err)
	}
	var anchors, recent []string
	for _, a := range b.Anchors {
		anchors = append(anchors, a.L1)
	}
	for _, it := range b.Recent.Items {
		recent = append(recent, it.L1)
	}
	if want := []string{issues[5], spec}; !slices.Equal(anchors, want) {
		t.Fatalf("anchors = %v, want %v", anchors, want)
	}
	if want := []string{issues[4], issues[3], issues[2], issues[1], issues[0]}; !slices.Equal(recent, want) {
		t.Errorf("recent = %v, want the five other issues, newest first: %v", recent, want)
	}
	if b.Anchors[0].PinnedBy != "kyle" || b.Anchors[1].PinnedBy != "" || b.Anchors[1].Kind != string(l1.KindWikiSection) {
		t.Errorf("anchors = %+v, want the pinned issue with its pinner, then the spec with none", b.Anchors)
	}
	if b.Recent.LastActivity != "2026-09-05T20:00:00Z" {
		t.Errorf("last_activity = %q, want the spec's: an anchor's activity is still the scope's", b.Recent.LastActivity)
	}
}

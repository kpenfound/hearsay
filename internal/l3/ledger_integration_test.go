//go:build integration

package l3_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
)

// operator is a configuration in which kyle may merge, split and undo in any
// scope: no authority is configured, so anyone may ratify by hand.
func operator(t *testing.T) config.Repo {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/eng.yaml":     "id: eng\nsources: [github]\n",
		"principals/p.yaml":   "- id: kyle\n  identities: [{source: github, native_id: u1}]\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	return repo
}

func operateOn(t *testing.T, pool *pgxpool.Pool, repo config.Repo, req l2.OperationRequest) l2.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req.Principal = "kyle"
	op, err := l2.Operate(ctx, pool, repo, req)
	if err != nil {
		t.Fatalf("Operate(%+v) = %v", req, err)
	}
	return op
}

// The current stances and the bundle serve topics as the topic ledger makes
// them, and withhold what the reader may not read of them.
func TestCurrentStancesFollowTheTopicLedger(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	graph := l2.New(pool)
	repo := operator(t)
	src := newSource()
	e, f := "code:"+src+":e", "code:"+src+":f"
	for _, id := range []string{e, f} {
		if err := graph.PutEntity(ctx, l2.Entity{ID: id, Type: l2.TypeModule, Origin: l2.OriginConfig}); err != nil {
			t.Fatal(err)
		}
	}
	topic := func(name, about string) l2.Topic {
		t.Helper()
		opener := putDoc(t, pool, src, "open-"+name, l1.KindIssue, 0, about, public)
		tp := l2.Topic{ID: l2.TopicID(src, opener, 0, name), Scope: src, Name: name, About: []string{about}, ACL: public, OpenedBy: opener}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		return tp
	}
	stance := func(tp l2.Topic, artifact string, acl connector.ACL, position string, hour int) l2.Stance {
		t.Helper()
		doc := putDoc(t, pool, src, artifact, l1.KindIssue, 0, e, acl)
		at := day.Add(time.Duration(hour) * time.Hour)
		st, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: acl,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	a := topic("the lock", e)
	b := topic("who holds the lock", f)
	stance(a, "a1", public, "the queue takes the lock", 1)
	b1 := stance(b, "b1", private, "the engine takes the lock", 2)
	stance(b, "b2", public, "the engine keeps the lock", 3)

	type row struct{ ID, Topic, Current, Supersedes string }
	read := func(reader l1.Reader, entity string) ([]row, int) {
		t.Helper()
		got, withheld, _, err := l3.New(pool).CurrentStances(ctx, reader, entity, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows := []row{}
		for _, c := range got {
			rows = append(rows, row{c.Topic.ID, c.Topic.Name, c.Stance.Position, c.Supersedes})
		}
		slices.SortFunc(rows, func(x, y row) int {
			if x.Topic < y.Topic {
				return -1
			}
			return 1
		})
		return rows, withheld
	}
	type reads struct {
		rows     [4][]row
		withheld [4]int
	}
	all := func() reads {
		var r reads
		for i, c := range []struct {
			reader l1.Reader
			entity string
		}{{kyle, e}, {kyle, f}, {sam, e}, {sam, f}} {
			r.rows[i], r.withheld[i] = read(c.reader, c.entity)
		}
		return r
	}
	before := all()

	// Merged, one topic is about both entities and stands on every stance of
	// both; sam reads it, but not the private stance the current one replaced.
	merged := operateOn(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Into: a.ID, From: b.ID})
	kyles := []row{{a.ID, "the lock", "the engine keeps the lock", "the engine takes the lock"}}
	sams := []row{{a.ID, "the lock", "the engine keeps the lock", ""}}
	for _, entity := range []string{e, f} {
		if got, _ := read(kyle, entity); !slices.Equal(got, kyles) {
			t.Errorf("kyle's stances on %s = %+v, want %+v", entity, got, kyles)
		}
		if got, _ := read(sam, entity); !slices.Equal(got, sams) {
			t.Errorf("sam's stances on %s = %+v, want %+v", entity, got, sams)
		}
	}
	b0, _, err := bundle.New(pool).Assemble(ctx, sam, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(b0.Stances) != 1 || b0.Stances[0].TopicID != a.ID {
		t.Errorf("sam's bundle on %s = %+v, want the one merged topic", f, b0.Stances)
	}

	// Split, the private stance is a topic of its own that sam may not read:
	// withheld from sam, and the stance it superseded is no position of a's.
	split := operateOn(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Topic: a.ID, Name: "who took the lock", Stances: []string{b1.ID}})
	s := split.Topics[1]
	want := []row{{a.ID, "the lock", "the engine keeps the lock", ""}, {s, "who took the lock", "the engine takes the lock", ""}}
	if got, withheld := read(kyle, f); !slices.Equal(got, want) || withheld != 0 {
		t.Errorf("kyle's stances on %s after the split = %+v (%d withheld), want %+v", f, got, withheld, want)
	}
	if got, withheld := read(sam, f); !slices.Equal(got, sams) || withheld != 1 {
		t.Errorf("sam's stances on %s after the split = %+v (%d withheld), want %+v and the split's topic withheld", f, got, withheld, sams)
	}

	// Undone, every read is as it was.
	operateOn(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Undoes: split.ID})
	operateOn(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Undoes: merged.ID})
	if after := all(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("after the undos the stances are %+v, want them as before: %+v", after, before)
	}
}

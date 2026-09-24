//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// `hearsay topics` end to end over a scratch database: what each person
// lists, what they may merge, split and undo, what the ledger shows them, and
// that a topic, stance or operation they may not read is answered exactly as
// one that does not exist.
//
// kyle reads everything and may ratify; sam reads only public evidence and may
// ratify; viv reads what sam does and may not ratify. Scope `api` holds two
// public topics, each with one public and one stance only kyle may read;
// scope `vault` holds a topic only kyle may read.
func TestTopicsCommands(t *testing.T) {
	url, pool := deleteDatabase(t, "topics_cli_")
	ctx := t.Context()
	configPath := writeConfig(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"principals/p.yaml": "- id: kyle\n  identities: [{source: github, native_id: u1}]\n" +
			"- id: sam\n  identities: [{source: github, native_id: u2}]\n" +
			"- id: viv\n  identities: [{source: github, native_id: u3}]\n",
		"authority/a.yaml": "scope: \"*\"\nratified_by:\n  principals: [kyle, sam]\n",
	})
	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "github", NativeID: "u1"}}
	when := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	docs := l1.New(pool)
	graph := l2.New(pool)
	put := func(name string, acl connector.ACL) string {
		t.Helper()
		doc := l1.Document{ID: l1.DocID("github", name), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
			Source: l1.Source{System: "github", NativeID: name}, L0Refs: []string{"evt:github:" + name},
			Time: l1.Times{Created: when, Updated: when, LastActivity: when}, ACL: acl,
			Text: name, RawText: name, Body: l1.Body{Summary: name, OutcomeKind: l1.OutcomeDecided}}
		if _, err := docs.Put(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	open := func(scope, name string, acl connector.ACL) l2.Topic {
		t.Helper()
		doc := put(scope+"-"+strings.ReplaceAll(name, " ", "-"), acl)
		topic := l2.Topic{ID: l2.TopicID(scope, doc, 0, name), Scope: scope, Name: name, ACL: acl, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		return topic
	}
	hour := 0
	stance := func(topic l2.Topic, name string, acl connector.ACL) string {
		t.Helper()
		hour++
		doc := put(name, acl)
		at := when.Add(time.Duration(hour) * time.Hour)
		st := l2.Stance{ID: l2.StanceID(topic.ID, doc, name, at, l2.TierInferred), TopicID: topic.ID, Position: name,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: acl}
		stored, _, err := graph.AppendStance(ctx, st, at)
		if err != nil {
			t.Fatal(err)
		}
		return stored.ID
	}
	lock := open("api", "the lock", public)
	holder := open("api", "who holds the lock", public)
	vault := open("vault", "the vault key", private)
	a1, a2 := stance(lock, "queue takes the lock", public), stance(lock, "secret: kyle takes the lock", private)
	b1, b2 := stance(holder, "engine takes the lock", public), stance(holder, "secret: engine keeps it", private)
	v1 := stance(vault, "the key is in the drawer", private)
	stance(vault, "the key is under the mat", private)

	topics := func(who string, args ...string) (string, error) {
		t.Helper()
		var out, stderr bytes.Buffer
		err := run(ctx, append([]string{"topics", "--database-url", url, "--config", configPath, "--principal", who}, args...), &out, &stderr)
		return out.String(), err
	}
	ledger := func() []l2.Operation {
		t.Helper()
		ops, err := graph.Operations(ctx, l2.OperationFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return ops
	}
	// mustFail runs a command that must be refused, print nothing and record
	// nothing, and returns its error message.
	mustFail := func(who string, args ...string) string {
		t.Helper()
		before := len(ledger())
		out, err := topics(who, args...)
		if err == nil {
			t.Fatalf("topics %v as %s succeeded, printing %q", args, who, out)
		}
		if out != "" {
			t.Errorf("topics %v as %s failed and printed %q", args, who, out)
		}
		if after := len(ledger()); after != before {
			t.Errorf("topics %v as %s failed and recorded %d operations", args, who, after-before)
		}
		return err.Error()
	}
	// recorded runs a command that must record one operation and print its id.
	recorded := func(who string, args ...string) l2.Operation {
		t.Helper()
		out, err := topics(who, args...)
		if err != nil {
			t.Fatalf("topics %v as %s = %v", args, who, err)
		}
		ops := ledger()
		last := ops[len(ops)-1]
		if want := fmt.Sprintf("%d\n", last.ID); out != want {
			t.Fatalf("topics %v as %s printed %q, want the recorded operation's id %q", args, who, out, want)
		}
		return last
	}
	// same asserts that two refusals read the same once the id each was asked
	// about is taken out: a hidden thing and a missing one are one answer.
	same := func(hidden, hiddenID, missing, missingID string) {
		t.Helper()
		if strings.ReplaceAll(hidden, hiddenID, "ID") != strings.ReplaceAll(missing, missingID, "ID") {
			t.Errorf("a hidden and a missing resource are told apart:\nhidden  %s\nmissing %s", hidden, missing)
		}
	}

	t.Run("list", func(t *testing.T) {
		out, err := topics("kyle", "list", "api")
		want := []string{"ID STANCES NAME", lock.ID + " 2 the lock", holder.ID + " 2 who holds the lock"}
		if err != nil || !slices.Equal(table(out), want) {
			t.Errorf("kyle's list = %q, %v; want %q", out, err, want)
		}
		out, err = topics("sam", "list", "api")
		want = []string{"ID STANCES NAME", lock.ID + " 1 the lock", holder.ID + " 1 who holds the lock"}
		if err != nil || !slices.Equal(table(out), want) {
			t.Errorf("sam's list = %q, %v; want %q", out, err, want)
		}
		hidden, err := topics("sam", "list", "vault")
		if err != nil || strings.Contains(hidden, vault.ID) || strings.Contains(hidden, "vault key") {
			t.Errorf("sam's list of vault = %q, %v; want nothing of it", hidden, err)
		}
		missing, err := topics("sam", "list", "nowhere")
		if err != nil || hidden != missing {
			t.Errorf("sam's list of a scope with nothing readable %q differs from a scope that does not exist %q (%v)", hidden, missing, err)
		}
	})

	var merge, split l2.Operation
	t.Run("merge", func(t *testing.T) {
		msg := mustFail("viv", "merge", holder.ID, lock.ID)
		if !strings.Contains(msg, "may not ratify by hand") {
			t.Errorf("viv's merge = %s, want refused as no ratifier", msg)
		}
		same(mustFail("sam", "merge", vault.ID, lock.ID), vault.ID, mustFail("sam", "merge", "topic:nope", lock.ID), "topic:nope")
		same(mustFail("sam", "merge", lock.ID, vault.ID), vault.ID, mustFail("sam", "merge", lock.ID, "topic:nope"), "topic:nope")
		// kyle may read both, and they are in two scopes.
		if msg := mustFail("kyle", "merge", vault.ID, lock.ID); !strings.Contains(msg, "stays inside one scope") {
			t.Errorf("a merge across scopes = %s", msg)
		}
		if msg := mustFail("kyle", "merge", lock.ID, lock.ID); !strings.Contains(msg, "merged into itself") {
			t.Errorf("a merge into itself = %s", msg)
		}
		merge = recorded("kyle", "merge", holder.ID, lock.ID)
		if merge.Kind != l2.OperationMerge || !slices.Equal(merge.Topics, []string{lock.ID, holder.ID}) || merge.Principal != "kyle" {
			t.Errorf("the merge recorded %+v", merge)
		}
	})

	t.Run("split", func(t *testing.T) {
		same(mustFail("sam", "split", lock.ID, "--stance", b2, "--name", "engine"), b2,
			mustFail("sam", "split", lock.ID, "--stance", "stance:nope", "--name", "engine"), "stance:nope")
		same(mustFail("sam", "split", vault.ID, "--stance", v1, "--name", "engine"), vault.ID,
			mustFail("sam", "split", "topic:nope", "--stance", v1, "--name", "engine"), "topic:nope")
		if msg := mustFail("sam", "split", holder.ID, "--stance", b1, "--name", "engine"); !strings.Contains(msg, "merged away") {
			t.Errorf("a split of a topic merged away = %s", msg)
		}
		if msg := mustFail("kyle", "split", lock.ID, "--stance", a1, "--stance", a2, "--stance", b1, "--stance", b2, "--name", "all"); !strings.Contains(msg, "leaves it empty") {
			t.Errorf("a split of every stance = %s", msg)
		}
		split = recorded("sam", "split", lock.ID, "--stance", b1, "--name", "engine")
		if split.Kind != l2.OperationSplit || split.Name != "engine" || !slices.Equal(split.Stances, []string{b1}) || split.Principal != "sam" {
			t.Errorf("the split recorded %+v", split)
		}
	})
	vaultSplit := recorded("kyle", "split", vault.ID, "--stance", v1, "--name", "secret half")

	opsJSON := func(who string, args ...string) []operationRecord {
		t.Helper()
		out, err := topics(who, append([]string{"ops", "--json"}, args...)...)
		if err != nil {
			t.Fatalf("ops --json as %s = %v", who, err)
		}
		var records []operationRecord
		if err := json.Unmarshal([]byte(out), &records); err != nil {
			t.Fatalf("ops --json printed %s: %v", out, err)
		}
		return records
	}
	ids := func(records []operationRecord) []int64 {
		out := []int64{}
		for _, r := range records {
			out = append(out, r.ID)
		}
		return out
	}

	t.Run("ops", func(t *testing.T) {
		kyle := opsJSON("kyle")
		if got, want := ids(kyle), []int64{merge.ID, split.ID, vaultSplit.ID}; !slices.Equal(got, want) {
			t.Fatalf("kyle's ops = %v, want %v", got, want)
		}
		wantStances := []string{b1, b2}
		slices.Sort(wantStances)
		if !slices.Equal(kyle[0].Stances, wantStances) {
			t.Errorf("kyle sees the merge cover stances %v, want %v", kyle[0].Stances, wantStances)
		}
		sam := opsJSON("sam")
		if got, want := ids(sam), []int64{merge.ID, split.ID}; !slices.Equal(got, want) {
			t.Fatalf("sam's ops = %v, want %v: the vault's split is not theirs to read", got, want)
		}
		wantMerge := operationRecord{ID: merge.ID, Kind: "merge", Scope: "api", Principal: "kyle", At: merge.At,
			Topics: []string{lock.ID, holder.ID}, Stances: []string{b1}}
		if got := sam[0]; !got.At.Equal(wantMerge.At) || fmt.Sprint(got.Topics, got.Stances, got.ID, got.Kind, got.Scope, got.Principal, got.Undone) !=
			fmt.Sprint(wantMerge.Topics, wantMerge.Stances, wantMerge.ID, wantMerge.Kind, wantMerge.Scope, wantMerge.Principal, false) {
			t.Errorf("sam's merge record = %+v, want %+v: a stance they may not read is not named", got, wantMerge)
		}
		if got := sam[1]; got.Name != "engine" || got.Kind != "split" || got.Principal != "sam" {
			t.Errorf("sam's split record = %+v", got)
		}
		if got := opsJSON("kyle", "--scope", "vault"); !slices.Equal(ids(got), []int64{vaultSplit.ID}) {
			t.Errorf("kyle's ops --scope vault = %v", ids(got))
		}
		if got := opsJSON("kyle", "--since", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); len(got) != 0 {
			t.Errorf("ops --since an hour from now = %v", ids(got))
		}
		if got := opsJSON("kyle", "--since", merge.At.Add(-time.Second).Format(time.RFC3339)); len(got) != 3 {
			t.Errorf("ops --since the merge = %v", ids(got))
		}

		text, err := topics("sam", "ops")
		if err != nil {
			t.Fatal(err)
		}
		lines := table(text)
		if len(lines) != 3 || lines[0] != "ID TIME KIND SCOPE PRINCIPAL UNDOES UNDONE_BY TOPICS STANCES NAME" {
			t.Fatalf("sam's ops = %q", text)
		}
		wantLine := fmt.Sprintf("%d %s merge api kyle - - %s,%s %s", merge.ID, merge.At.Format(time.RFC3339), lock.ID, holder.ID, b1)
		if got := lines[1]; got != wantLine {
			t.Errorf("sam's merge line = %q, want %q", got, wantLine)
		}
		if strings.Contains(text, "secret half") || strings.Contains(text, vault.ID) || strings.Contains(text, b2) {
			t.Errorf("sam's ops show what they may not read: %s", text)
		}
		hidden, err := topics("sam", "ops", "--scope", "vault")
		if err != nil {
			t.Fatal(err)
		}
		if missing, err := topics("sam", "ops", "--scope", "nowhere"); err != nil || missing != hidden {
			t.Errorf("sam's ops of a scope with nothing readable %q differ from a scope that does not exist %q (%v)", hidden, missing, err)
		}
	})

	t.Run("undo", func(t *testing.T) {
		same(mustFail("sam", "undo", fmt.Sprint(vaultSplit.ID)), fmt.Sprint(vaultSplit.ID), mustFail("sam", "undo", "999999"), "999999")
		if msg := mustFail("viv", "undo", fmt.Sprint(split.ID)); !strings.Contains(msg, "may not ratify by hand") {
			t.Errorf("viv's undo = %s", msg)
		}
		// A split of the stance only kyle may read makes a topic only kyle
		// may read: it is in the way of undoing the merge, and sam is not
		// told which operation it is.
		private := recorded("kyle", "split", lock.ID, "--stance", a2, "--name", "private half")
		if msg := mustFail("kyle", "undo", fmt.Sprint(merge.ID)); !strings.HasSuffix(msg, fmt.Sprintf("undo them first: operation %d, %d", split.ID, private.ID)) {
			t.Errorf("kyle's undo under two later splits = %s", msg)
		}
		if msg := mustFail("sam", "undo", fmt.Sprint(merge.ID)); !strings.HasSuffix(msg, fmt.Sprintf("undo them first: operation %d", split.ID)) {
			t.Errorf("sam's undo under two later splits = %s, want a conflict naming only %d", msg, split.ID)
		}
		if got, want := ids(opsJSON("sam")), []int64{merge.ID, split.ID}; !slices.Equal(got, want) {
			t.Errorf("sam's ops = %v, want %v: kyle's split is not theirs to read", got, want)
		}
		// kyle's split is also in the way of undoing sam's, and it is all
		// that is: sam is told there is a conflict, and nothing about it.
		msg := mustFail("sam", "undo", fmt.Sprint(split.ID))
		if !strings.Contains(msg, "conflicting topic operation") || !strings.Contains(msg, "still in force") || strings.Contains(msg, fmt.Sprint("operation ", private.ID)) {
			t.Errorf("sam's undo under a split they may not read = %s, want a conflict naming no operation", msg)
		}
		undoPrivate := recorded("kyle", "undo", fmt.Sprint(private.ID))
		undoSplit := recorded("sam", "undo", fmt.Sprint(split.ID))
		if undoSplit.Kind != l2.OperationUndo || undoSplit.Undoes != split.ID || undoSplit.Principal != "sam" {
			t.Errorf("the undo recorded %+v", undoSplit)
		}
		if msg := mustFail("sam", "undo", fmt.Sprint(undoSplit.ID)); !strings.Contains(msg, "an undo is not undone") {
			t.Errorf("an undo of an undo = %s", msg)
		}
		undoMerge := recorded("kyle", "undo", fmt.Sprint(merge.ID))
		if msg := mustFail("kyle", "undo", fmt.Sprint(merge.ID)); !strings.HasSuffix(msg, fmt.Sprintf("already undone by: operation %d", undoMerge.ID)) {
			t.Errorf("a second undo = %s", msg)
		}
		byID := map[int64]operationRecord{}
		for _, r := range opsJSON("sam") {
			byID[r.ID] = r
		}
		for _, tt := range []struct {
			id, undoes, undoneBy int64
		}{
			{merge.ID, 0, undoMerge.ID}, {split.ID, 0, undoSplit.ID}, {undoSplit.ID, split.ID, 0},
			{undoPrivate.ID, private.ID, 0}, {undoMerge.ID, merge.ID, 0},
		} {
			r, ok := byID[tt.id]
			if !ok || r.Undoes != tt.undoes || r.UndoneBy != tt.undoneBy || r.Undone != (tt.undoneBy != 0) {
				t.Errorf("sam's record of operation %d = %+v (listed %v), want undoes %d and undone by %d", tt.id, r, ok, tt.undoes, tt.undoneBy)
			}
		}
		out, err := topics("kyle", "list", "api")
		if want := []string{"ID STANCES NAME", lock.ID + " 2 the lock", holder.ID + " 2 who holds the lock"}; err != nil || !slices.Equal(table(out), want) {
			t.Errorf("kyle's list after the undos = %q, %v; want both topics back as they were", out, err)
		}
	})
}

// table is a tabwriter's output, a line per row with its cells one space
// apart.
func table(out string) []string {
	var lines []string
	for line := range strings.Lines(out) {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	return lines
}

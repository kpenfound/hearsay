//go:build integration

package distiller_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/poll"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func TestAnOperatorDeletionTakesAPastedSecretOutOfEveryRead(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	ctx := t.Context()
	ingest(t, pool, append(fixtureEvents(src), secretComment(src)))
	secretEvent := connector.EventID(src, secretCommentID)

	stop := startDistiller(t, pool, src)
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()

	docs := l1.New(pool)
	id := l1.DocID(src, repo+"#12")
	waitForIssue := func(what string, ok func(l1.Stored) bool) l1.Stored {
		t.Helper()
		var found l1.Stored
		poll.Within(t, "the issue's document to have "+what, 60*time.Second, func() bool {
			doc, err := docs.Get(ctx, id)
			found = doc
			return err == nil && ok(doc)
		})
		return found
	}
	before := waitForIssue("quoted the secret", func(doc l1.Stored) bool { return slices.Contains(doc.L0Refs, secretEvent) })
	if !strings.Contains(before.RawText, pastedSecret) || !strings.Contains(before.Text, pastedSecret) {
		t.Fatalf("the secret is not in the issue's document before the deletion:\n%s\n%s", before.Text, before.RawText)
	}

	t.Setenv("HEARSAY_TEST_DELETE_KYLE", "test-delete-kyle-credential")
	t.Setenv("HEARSAY_TEST_DELETE_SAM", "test-delete-sam-credential")
	repoWithAPI := testRepo(src)
	for i, env := range []string{"HEARSAY_TEST_DELETE_KYLE", "HEARSAY_TEST_DELETE_SAM"} {
		repoWithAPI.Principals[i].TokenEnv = env
		repoWithAPI.Principals[i].Grant = principal.Grant{Scopes: principal.AllScopes()}
	}
	calls, err := api.NewCalls(pool, repoWithAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	kyle := api.Caller{Principal: "kyle"}
	call := func(name string, args any) ([]byte, error) {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return calls.Call(ctx, kyle, name, raw)
	}

	reads := []struct {
		name   string
		args   any
		needle string
	}{
		{"get_l1", map[string]string{"id": id}, pastedSecret},
		{"search", map[string]string{"query": pastedSecret}, pastedSecret},
		{"search", map[string]string{"query": "vault passphrase"}, pastedSecret},
		{"get_bundle", map[string]string{"scope": before.Scope[0]}, "vault passphrase"},
	}
	for _, r := range reads {
		body, err := call(r.name, r.args)
		if err != nil || !strings.Contains(string(body), r.needle) {
			t.Fatalf("%s(%v) before the deletion = %v, %s; want the secret, or this test proves nothing", r.name, r.args, err, body)
		}
	}
	if body, err := call("get_l0", map[string]string{"id": secretEvent}); err != nil || !strings.Contains(string(body), pastedSecret) {
		t.Fatalf("get_l0 before the deletion = %v, %s", err, body)
	}

	applied, err := deletion.Apply(ctx, pool, testRepo(src), deletion.Selector{Event: secretEvent}, "a pasted secret", "kyle")
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if !slices.Equal(applied.Documents, []string{id}) {
		t.Errorf("Apply().Documents = %v, want only the issue", applied.Documents)
	}

	after := waitForIssue("was re-distilled without the secret", func(doc l1.Stored) bool { return !slices.Contains(doc.L0Refs, secretEvent) })
	if !strings.Contains(after.Text, "Two people confirmed it") {
		t.Errorf("the issue was not re-distilled from the recorded answer for the repository without the paste:\n%s", after.Text)
	}
	if len(after.L0Refs) != len(before.L0Refs)-1 {
		t.Errorf("L0Refs = %v, want %v less the deleted comment", after.L0Refs, before.L0Refs)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM l0_events WHERE source = $1 AND payload::text LIKE '%' || $2 || '%'`, src, pastedSecret).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d L0 rows still hold the secret (%v)", rows, err)
	}
	events := l0.New(pool)
	listed, err := events.List(ctx, l0.ListOptions{Filter: l0.Filter{Source: src}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	current, err := events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: src, Thread: repo + "#12"}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := events.Changes(ctx, l0.Cursor{}, l0.Filter{Source: src}, l0.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"List": listed, "Current": current, "Changes": changes} {
		if encoded, _ := json.Marshal(v); strings.Contains(string(encoded), pastedSecret) || strings.Contains(string(encoded), secretCommentID) {
			t.Errorf("l0 %s still returns the deleted comment", name)
		}
	}
	if _, err := events.Get(ctx, secretEvent); !errors.Is(err, l0.ErrDeleted) {
		t.Errorf("Get(deleted) = %v, want ErrDeleted", err)
	}

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM l1_docs WHERE source = $1 AND (text || raw_text || body::text) LIKE '%' || $2 || '%'`, src, pastedSecret).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d L1 rows still hold the secret (%v)", rows, err)
	}
	for _, r := range reads {
		body, err := call(r.name, r.args)
		if err != nil || strings.Contains(string(body), r.needle) {
			t.Errorf("%s(%v) after the deletion = %v, %s; want it without the secret", r.name, r.args, err, body)
		}
	}
	if body, err := call("get_l0", map[string]string{"id": secretEvent}); err == nil || strings.Contains(string(body), pastedSecret) {
		t.Errorf("get_l0 after the deletion = %v, %s; want not found", err, body)
	}
}

//go:build integration

package deletion_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
)

func state(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
  'l0', (SELECT jsonb_agg(e ORDER BY e.id) FROM l0_events e),
  'deletions', (SELECT jsonb_agg(d ORDER BY d.id) FROM l0_deletions d),
  'jobs', (SELECT jsonb_agg(q ORDER BY q.id) FROM queue_job q))::text`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestApplyIsAllOrNothing(t *testing.T) {
	pool := dbtest.Scratch(t, "deletion")
	ctx := t.Context()
	store := l0.New(pool)
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := func(artifact, text string) connector.Event {
		return connector.Event{
			Source: "chat", NativeID: artifact, Kind: connector.KindMessage, Time: when,
			Payload: connector.Payload{
				Artifact: artifact, Text: text, Thread: "thread", Parent: "thread",
				Container: connector.Container{Kind: connector.ContainerChannel, NativeID: "team"},
				Author:    &connector.Identity{Source: "chat", Kind: connector.IdentityUser, NativeID: "u1"},
			},
			ACL: connector.ACL{{Kind: connector.ACLPublic}},
		}
	}
	for _, ev := range []connector.Event{msg("thread", "a thread"), msg("reply", "the passphrase is tangerine-otter-42")} {
		if _, err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,artifact_class)
VALUES('l1:chat:thread','chat_thread','chat','thread',ARRAY['evt:chat:thread','evt:chat:reply'],$1,$1,$1,'[{"kind":"public"}]','text','raw','{}','none','chat_thread')`, when); err != nil {
		t.Fatal(err)
	}
	repo := config.Repo{Principals: []principal.Principal{
		{ID: "pat", Kind: principal.KindHuman},
		{ID: "bot", Kind: principal.KindAgent},
		{ID: "ops", Kind: principal.KindTeam},
	}}
	sel := deletion.Selector{Event: "evt:chat:reply"}
	before := state(t, pool)

	for _, operator := range []string{"", "nobody", "bot", "ops"} {
		if _, err := deletion.Apply(ctx, pool, repo, sel, "a pasted secret", operator); err == nil {
			t.Errorf("Apply(operator %q) succeeded, want a refusal", operator)
		}
		if got := state(t, pool); got != before {
			t.Fatalf("Apply(operator %q) wrote something:\nbefore %s\nafter  %s", operator, before, got)
		}
	}

	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, on string }{
		{"writing the deletion event", "BEFORE INSERT ON l0_events"},
		{"recording the deletion", "BEFORE INSERT ON l0_deletions"},
		{"redacting the events", "BEFORE UPDATE ON l0_events"},
		{"enqueueing the re-distillation", "BEFORE INSERT ON queue_job"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			table := tt.on[strings.LastIndex(tt.on, " ")+1:]
			if _, err := pool.Exec(ctx, `CREATE TRIGGER fail `+tt.on+` FOR EACH ROW EXECUTE FUNCTION fail_write()`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.Exec(context.Background(), `DROP TRIGGER fail ON `+table); err != nil {
					t.Fatal(err)
				}
			}()
			_, err := deletion.Apply(ctx, pool, repo, sel, "a pasted secret", "pat")
			if err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("Apply() = %v, want the injected failure", err)
			}
			if got := state(t, pool); got != before {
				t.Errorf("a failed Apply left writes behind:\nbefore %s\nafter  %s", before, got)
			}
		})
	}

	applied, err := deletion.Apply(ctx, pool, repo, sel, "a pasted secret", "pat")
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if applied.Operator != "pat" || !slices.Equal(applied.Events, []string{"evt:chat:reply"}) || !slices.Equal(applied.Documents, []string{"l1:chat:thread"}) {
		t.Errorf("Apply() = %+v", applied)
	}
	var (
		operator, reason string
		events, docs     []string
		retraction       string
	)
	if err := pool.QueryRow(ctx, `SELECT operator, reason, events, documents, retraction FROM l0_deletions WHERE id = $1`, applied.Deletion).
		Scan(&operator, &reason, &events, &docs, &retraction); err != nil {
		t.Fatal(err)
	}
	if operator != "pat" || reason != "a pasted secret" || !slices.Equal(events, applied.Events) || !slices.Equal(docs, applied.Documents) || retraction != applied.Retraction {
		t.Errorf("the deletion record = %s %q %v %v %s", operator, reason, events, docs, retraction)
	}
	var jobs []string
	rows, err := pool.Query(ctx, `SELECT kind || ' ' || target_id FROM queue_job ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(jobs, []string{"distill l1:chat:thread"}) {
		t.Errorf("jobs = %v, want one distill job for the thread", jobs)
	}
	if got := state(t, pool); strings.Contains(got, "tangerine-otter-42") {
		t.Errorf("the secret is still in L0 after the deletion: %s", got)
	}
}

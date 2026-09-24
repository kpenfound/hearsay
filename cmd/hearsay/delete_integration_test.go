//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
)

// deleteDatabase is a migrated scratch database: the preview test compares the
// whole of every table before and after, which only a database of its own
// allows.
func deleteDatabase(t *testing.T, prefix string) (string, *pgxpool.Pool) {
	t.Helper()
	adminURL := os.Getenv("HEARSAY_DATABASE_URL")
	if adminURL == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	ctx := t.Context()
	admin, err := db.Open(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropper, err := db.Open(context.Background(), adminURL)
		if err != nil {
			t.Error(err)
			return
		}
		defer dropper.Close()
		_, _ = dropper.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})
	url := swapDatabaseName(adminURL, name)
	m, err := db.NewMigrator(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return url, pool
}

func TestDeletePreviewSelectorsAndNoWrites(t *testing.T) {
	url, pool := deleteDatabase(t, "delete_preview_")
	ctx := t.Context()
	configPath := writeConfig(t, map[string]string{
		"sources/chat.yaml":   "id: chat\ntype: discord\ncontainers: [team]\n",
		"sources/code.yaml":   "id: code\ntype: github\ncontainers: [acme/api]\n",
		"scopes/all.yaml":     "id: all\nsources: [chat, code]\n",
		"principals/pat.yaml": "id: pat\nidentities: [{source: chat, native_id: u1}, {source: code, native_id: u9}]\n",
	})
	acl := `[ {"kind":"public"} ]`
	when := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	putEvent := func(id, source, native, artifact, author string) {
		t.Helper()
		payload := `{"artifact":"` + artifact + `","author":{"source":"` + source + `","kind":"user","native_id":"` + author + `"}}`
		_, err := pool.Exec(ctx, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl) VALUES($1,$2,$3,'message',$4,$5,$6,$7)`, id, source, native, artifact, when, payload, acl)
		if err != nil {
			t.Fatal(err)
		}
	}
	putEvent("evt:chat:thread", "chat", "thread", "thread", "other")
	putEvent("evt:chat:reply", "chat", "reply", "reply", "u1")
	_, err := pool.Exec(ctx, `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,revision_edited_at,occurred_at,payload,acl) VALUES('evt:chat:reply@2','chat','reply@2','message','reply','2',$1,$1,'{"artifact":"reply","author":{"source":"chat","kind":"user","native_id":"u1"}}',$2)`, when, acl)
	if err != nil {
		t.Fatal(err)
	}
	putEvent("evt:code:pr", "code", "pr", "pr", "u9")
	putDoc := func(id, source, native string, refs []string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,artifact_class) VALUES($1,'chat_thread',$2,$3,$4,$5,$5,$5,$6,'text','raw','{}','decided','chat_thread')`, id, source, native, refs, when, acl)
		if err != nil {
			t.Fatal(err)
		}
	}
	putDoc("l1:chat:thread", "chat", "thread", []string{"evt:chat:thread", "evt:chat:reply", "evt:chat:reply@2"})
	putDoc("l1:code:pr", "code", "pr", []string{"evt:code:pr"})
	_, err = pool.Exec(ctx, `INSERT INTO l2_topics(id,scope,name,acl,opened_by) VALUES('topic-1','all','choice',$1,'l1:chat:thread')`, acl)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO l2_stances(id,topic_id,position,stated_at,evidence,tier,acl) VALUES('stance-1','topic-1','yes',$1,ARRAY['l1:chat:thread'],'inferred',$2)`, when, acl)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO l2_alias_candidates(entity_id,alias,name,evidence,acl) VALUES('entity-1','alias-1','name',ARRAY['l1:chat:thread'],$1)`, acl)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO l2_alias_candidates(entity_id,alias,name,acl) VALUES('entity-2','alias-2','other',$1)`, acl)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO l2_alias_votes(entity_id,alias,doc_id,pr_doc_id) VALUES('entity-2','alias-2','l1:chat:thread','l1:code:pr')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO l2_pins(scope,l1,pinned_by,pinned_at) VALUES('all','l1:chat:thread','pat',$1)`, when)
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('l0',(SELECT jsonb_agg(e ORDER BY id) FROM l0_events e),'l1',(SELECT jsonb_agg(d ORDER BY id) FROM l1_docs d),'stances',(SELECT jsonb_agg(s ORDER BY id) FROM l2_stances s),'topics',(SELECT jsonb_agg(t ORDER BY id) FROM l2_topics t),'aliases',(SELECT jsonb_agg(a ORDER BY entity_id) FROM l2_alias_candidates a),'pins',(SELECT jsonb_agg(p ORDER BY scope) FROM l2_pins p))`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (deletion.Preview, string) {
		t.Helper()
		cmd := append([]string{"delete", "--database-url", url, "--config", configPath, "--reason", "operator request", "--json"}, args...)
		var out, stderr bytes.Buffer
		if err := run(ctx, cmd, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		var p deletion.Preview
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p, out.String()
	}
	p, raw := invoke("--event", "evt:chat:reply")
	if !strings.Contains(raw, `"counts":{"alias_candidates":2,"documents":1,"events":1,"pins":1,"stances":1,"topics":1}`) {
		t.Fatalf("JSON counts missing: %s", raw)
	}
	if !reflect.DeepEqual(p.Events, []string{"evt:chat:reply"}) || !reflect.DeepEqual(p.Documents, []string{"l1:chat:thread"}) || !reflect.DeepEqual(p.Stances, []string{"stance-1"}) || !reflect.DeepEqual(p.Topics, []string{"topic-1"}) || !reflect.DeepEqual(p.AliasCandidates, []string{"entity-1:alias-1", "entity-2:alias-2"}) || !reflect.DeepEqual(p.Pins, []string{"all:l1:chat:thread"}) {
		t.Fatalf("thread event preview = %+v", p)
	}
	p, _ = invoke("--artifact", "chat", "reply")
	if !reflect.DeepEqual(p.Events, []string{"evt:chat:reply", "evt:chat:reply@2"}) {
		t.Fatalf("artifact events = %v", p.Events)
	}
	p, _ = invoke("--author", "pat")
	if !reflect.DeepEqual(p.Events, []string{"evt:chat:reply", "evt:chat:reply@2", "evt:code:pr"}) || !reflect.DeepEqual(p.Documents, []string{"l1:chat:thread", "l1:code:pr"}) {
		t.Fatalf("principal preview = %+v", p)
	}
	p, _ = invoke("--author", "chat:u1")
	if len(p.Events) != 2 {
		t.Fatalf("source identity events = %v", p.Events)
	}
	var out, stderr bytes.Buffer
	if err := run(ctx, []string{"delete", "--database-url", url, "--reason", "operator request", "--event", "evt:chat:reply"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "L1 documents (1):") || !strings.Contains(out.String(), "l1:chat:thread") || !strings.Contains(out.String(), "Pins (1):") {
		t.Fatalf("text preview = %s", out.String())
	}
	var after []byte
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('l0',(SELECT jsonb_agg(e ORDER BY id) FROM l0_events e),'l1',(SELECT jsonb_agg(d ORDER BY id) FROM l1_docs d),'stances',(SELECT jsonb_agg(s ORDER BY id) FROM l2_stances s),'topics',(SELECT jsonb_agg(t ORDER BY id) FROM l2_topics t),'aliases',(SELECT jsonb_agg(a ORDER BY entity_id) FROM l2_alias_candidates a),'pins',(SELECT jsonb_agg(p ORDER BY scope) FROM l2_pins p))`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("preview changed the database")
	}
	for _, args := range [][]string{
		{"--event", "evt:chat:reply"},
		{"--reason", "x"},
		{"--reason", "x", "--event", "broken"},
		{"--reason", "x", "--event", "evt:chat:reply", "--author", "pat"},
		{"--reason", "x", "--artifact", "chat"},
		{"--reason", "x", "--author", "unknown"},
		{"--reason", "x", "--author", "chat:missing"},
	} {
		var out, stderr bytes.Buffer
		cmd := append([]string{"delete", "--database-url", url, "--config", configPath}, args...)
		if err := run(ctx, cmd, &out, &stderr); err == nil {
			t.Errorf("%v unexpectedly succeeded", args)
		}
	}
}

// The acceptance criteria for the operator: applying needs a configured human,
// named with --principal, and anything else is refused before a write — before
// the database is even opened. The preview needs none. `l0 get` names an
// operator deletion and returns no content, and a tombstone keeps its source.
func TestDeleteApplyNamesAConfiguredHumanOperator(t *testing.T) {
	url, pool := deleteDatabase(t, "delete_apply_")
	ctx := t.Context()
	configPath := writeConfig(t, map[string]string{
		"sources/chat.yaml":   "id: chat\ntype: discord\ncontainers: [team]\n",
		"scopes/all.yaml":     "id: all\nsources: [chat]\n",
		"principals/pat.yaml": "id: pat\nidentities: [{source: chat, native_id: u1}]\n",
		"principals/bot.yaml": "id: bot\nkind: agent\nclass: worker\nscopes: ['*']\nidentities: [{source: chat, native_id: b1}]\n",
	})
	const secret = "tangerine-otter-42"
	store := l0.New(pool)
	msg := func(kind connector.Kind, artifact, text string) connector.Event {
		return connector.Event{
			Source: "chat", NativeID: artifact, Kind: kind, Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			Payload: connector.Payload{
				Artifact: artifact, Text: text,
				Container: connector.Container{Kind: connector.ContainerChannel, NativeID: "team"},
				Author:    &connector.Identity{Source: "chat", Kind: connector.IdentityUser, NativeID: "u1"},
			},
			ACL: connector.ACL{{Kind: connector.ACLPublic}},
		}
	}
	tombstone := msg(connector.KindTombstone, "m3:tombstone", "")
	tombstone.Payload.Target, tombstone.Payload.Author = "m3", nil
	for _, ev := range []connector.Event{msg(connector.KindMessage, "reply", "the passphrase is "+secret), msg(connector.KindMessage, "m3", "gone at the source"), tombstone} {
		if _, err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('l0',(SELECT jsonb_agg(e ORDER BY id) FROM l0_events e),'deletions',(SELECT jsonb_agg(d ORDER BY id) FROM l0_deletions d),'jobs',(SELECT jsonb_agg(q ORDER BY id) FROM queue_job q))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	selector := []string{"delete", "--reason", "a pasted secret", "--event", "evt:chat:reply"}
	nowhere := "postgres://hearsay@nowhere.invalid:1/hearsay"
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no principal", []string{"--database-url", url, "--config", configPath, "--apply"}, "--apply needs --config and --principal"},
		{"no config", []string{"--database-url", url, "--apply", "--principal", "pat"}, "--apply needs --config and --principal"},
		{"unknown principal", []string{"--database-url", url, "--config", configPath, "--apply", "--principal", "nobody"}, "unknown principal"},
		{"agent principal", []string{"--database-url", url, "--config", configPath, "--apply", "--principal", "bot"}, "must be a configured human"},
		{"principal without apply", []string{"--database-url", url, "--config", configPath, "--principal", "pat"}, "only read with --apply"},
		// Refused before the database is opened: an unreachable one does not
		// change the answer.
		{"agent principal, no database", []string{"--database-url", nowhere, "--config", configPath, "--apply", "--principal", "bot"}, "must be a configured human"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			err := run(ctx, append(append([]string{}, selector...), tt.args...), &out, &stderr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() = %v, want a refusal saying %q", err, tt.want)
			}
			if got := snapshot(); got != before {
				t.Errorf("a refused apply wrote something:\nbefore %s\nafter  %s", before, got)
			}
		})
	}

	var out, stderr bytes.Buffer
	if err := run(ctx, append(append([]string{}, selector...), "--database-url", url), &out, &stderr); err != nil {
		t.Fatalf("the preview without --principal = %v", err)
	}
	if got := snapshot(); got != before {
		t.Fatal("the preview wrote something")
	}

	out.Reset()
	if err := run(ctx, append(append([]string{}, selector...), "--database-url", url, "--config", configPath, "--apply", "--principal", "pat", "--json"), &out, &stderr); err != nil {
		t.Fatalf("apply = %v", err)
	}
	var applied deletion.Applied
	if err := json.Unmarshal(out.Bytes(), &applied); err != nil {
		t.Fatalf("apply --json printed %s: %v", out.String(), err)
	}
	var operator string
	if err := pool.QueryRow(ctx, `SELECT operator FROM l0_deletions WHERE id = $1`, applied.Deletion).Scan(&operator); err != nil || operator != "pat" || applied.Operator != "pat" {
		t.Errorf("recorded operator = %q, %v; printed %q; want pat", operator, err, applied.Operator)
	}
	if strings.Contains(snapshot(), secret) {
		t.Error("the secret is still in L0")
	}

	get := func(id string) (string, error) {
		t.Helper()
		var out, stderr bytes.Buffer
		err := run(ctx, []string{"l0", "get", id, "--database-url", url}, &out, &stderr)
		return out.String(), err
	}
	printed, err := get("evt:chat:reply")
	if err == nil || !strings.Contains(err.Error(), applied.Deletion) || !strings.Contains(err.Error(), "by pat") ||
		!strings.Contains(err.Error(), "deleted by an operator") || strings.Contains(err.Error(), "tombstone") {
		t.Errorf("l0 get <deleted> = %v, want the deletion %s by pat and no tombstone", err, applied.Deletion)
	}
	if strings.Contains(printed, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
		t.Errorf("l0 get <deleted> printed the content: %s %v", printed, err)
	}
	if _, err := get("evt:chat:m3"); err == nil || !strings.Contains(err.Error(), "at source chat by evt:chat:m3:tombstone") || strings.Contains(err.Error(), "operator") {
		t.Errorf("l0 get <tombstoned> = %v, want the source's tombstone and no operator", err)
	}
}

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

	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/deletion"
)

func TestDeletePreviewSelectorsAndNoWrites(t *testing.T) {
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
	name := "delete_preview_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
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
	defer pool.Close()
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
	_, err = pool.Exec(ctx, `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,revision_edited_at,occurred_at,payload,acl) VALUES('evt:chat:reply@2','chat','reply@2','message','reply','2',$1,$1,'{"artifact":"reply","author":{"source":"chat","kind":"user","native_id":"u1"}}',$2)`, when, acl)
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
	p, _ := invoke("--event", "evt:chat:reply")
	if !reflect.DeepEqual(p.Events, []string{"evt:chat:reply"}) || !reflect.DeepEqual(p.Documents, []string{"l1:chat:thread"}) || !reflect.DeepEqual(p.Stances, []string{"stance-1"}) || !reflect.DeepEqual(p.Topics, []string{"topic-1"}) || !reflect.DeepEqual(p.AliasCandidates, []string{"entity-1:alias-1"}) || !reflect.DeepEqual(p.Pins, []string{"all:l1:chat:thread"}) {
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

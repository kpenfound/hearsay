//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestGesturesCommand(t *testing.T) {
	url, pool := deleteDatabase(t, "gestures_cli_")
	ctx := t.Context()
	path := writeConfig(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"principals/p.yaml": "- id: kyle\n  identities: [{source: github, native_id: u1}]\n" +
			"- id: sam\n  identities: [{source: github, native_id: u2}]\n" +
			"- id: viv\n  identities: [{source: github, native_id: u3}]\n" +
			"- id: bot\n  kind: agent\n  identities: [{source: github, native_id: u4}]\n  class: worker\n  scopes: [code:acme/api]\n  token_env: BOT_TOKEN\n",
		"authority/a.yaml": "scope: \"*\"\nratified_by:\n  principals: [kyle, sam]\n",
	})
	when := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "github", NativeID: "u1"}}
	put := func(artifact string, acl connector.ACL) string {
		t.Helper()
		id := l1.DocID("github", artifact)
		event := connector.EventID("github", artifact)
		aclJSON, err := json.Marshal(acl)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl) VALUES($1,'github',$2,'issue',$2,$3,$4,$5)`, event, artifact, when, `{"artifact":"`+artifact+`","container":{"source":"github","native_id":"acme/api"},"author":{"source":"github","kind":"user","native_id":"u1"}}`, aclJSON)
		if err != nil {
			t.Fatal(err)
		}
		doc := l1.Document{ID: id, Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue, Source: l1.Source{System: "github", NativeID: artifact}, L0Refs: []string{event}, Time: l1.Times{Created: when, Updated: when, LastActivity: when}, ACL: acl, Scope: []string{"code:acme/api"}, Text: artifact, RawText: artifact, Body: l1.Body{Summary: artifact, OutcomeKind: l1.OutcomeDecided}}
		if _, err := l1.New(pool).Put(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return id
	}
	visible, hidden := put("public", public), put("private", private)
	graph := l2.New(pool)
	for _, id := range []string{visible, hidden} {
		topic := l2.Topic{ID: l2.TopicID("api", id, 0, id), Scope: "api", Name: id, ACL: public, OpenedBy: id, About: []string{"api"}}
		if _, err := graph.OpenTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		st := l2.Stance{ID: l2.StanceID(topic.ID, id, "yes", when, l2.TierInferred), TopicID: topic.ID, Position: "yes", StatedAt: when, Evidence: []string{id}, Tier: l2.TierInferred, ACL: public}
		if _, _, err := graph.AppendStance(ctx, st, when); err != nil {
			t.Fatal(err)
		}
	}
	call := func(who string, args ...string) (string, error) {
		t.Helper()
		var out, stderr bytes.Buffer
		err := run(ctx, append([]string{"gestures", "--database-url", url, "--config", path, "--principal", who}, args...), &out, &stderr)
		return out.String(), err
	}
	ledger := func() []l2.Gesture {
		t.Helper()
		g, err := graph.Gestures(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	fail := func(who string, args ...string) string {
		t.Helper()
		before := len(ledger())
		out, err := call(who, args...)
		if err == nil || out != "" || len(ledger()) != before {
			t.Fatalf("%s %v = %q, %v, %d records", who, args, out, err, len(ledger()))
		}
		return err.Error()
	}
	record := func(who string, args ...string) l2.Gesture {
		t.Helper()
		out, err := call(who, args...)
		if err != nil {
			t.Fatalf("%s %v: %v", who, args, err)
		}
		g := ledger()[len(ledger())-1]
		if out != fmt.Sprintf("%d\n", g.ID) {
			t.Fatalf("printed %q, want gesture %d", out, g.ID)
		}
		return g
	}
	for _, who := range []string{"absent", "bot", "viv"} {
		if msg := fail(who, "ratify", visible); msg == "" {
			t.Fatal("empty refusal")
		}
	}
	missing := l1.DocID("github", "missing")
	hideErr := fail("sam", "ratify", hidden)
	missErr := fail("sam", "ratify", missing)
	if strings.ReplaceAll(hideErr, hidden, "ID") != strings.ReplaceAll(missErr, missing, "ID") {
		t.Fatalf("hidden %q differs from missing %q", hideErr, missErr)
	}
	if got := fail("sam", "pin", "--artifact", "github", "private"); !strings.Contains(got, "no document") {
		t.Fatal(got)
	}
	ratify := record("kyle", "ratify", "--artifact", "github", "public")
	if ratify.Action != l2.GestureRatify || ratify.Principal != "kyle" || !slices.Equal(ratify.Documents, []string{visible}) || len(ratify.Stances) != 1 {
		t.Fatalf("ratify = %+v", ratify)
	}
	secret := record("kyle", "demote", hidden)
	if got := fail("sam", "undo", strconv.FormatInt(secret.ID, 10)); !strings.Contains(got, "no gesture") {
		t.Fatal(got)
	}
	if got := fail("sam", "undo", "999999"); strings.ReplaceAll(got, "999999", "ID") != strings.ReplaceAll(fail("sam", "undo", strconv.FormatInt(secret.ID, 10)), strconv.FormatInt(secret.ID, 10), "ID") {
		t.Fatal("hidden and missing gesture differ")
	}
	listed, err := call("sam", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var records []gestureRecord
	if err := json.Unmarshal([]byte(listed), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != ratify.ID {
		t.Fatalf("sam ledger = %+v", records)
	}
	if out, err := call("sam", "list", "--scope", "other", "--json"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("scope filter = %q, %v", out, err)
	}
	if out, err := call("sam", "list", "--since", time.Now().Add(time.Hour).Format(time.RFC3339), "--json"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("since filter = %q, %v", out, err)
	}
	undo := record("sam", "undo", strconv.FormatInt(ratify.ID, 10))
	if undo.Undoes != ratify.ID || undo.Principal != "sam" {
		t.Fatalf("undo = %+v", undo)
	}
	pin := record("kyle", "pin", visible)
	if pin.Action != l2.GesturePin || len(pin.Pins) == 0 {
		t.Fatalf("pin = %+v", pin)
	}
	unpin := record("kyle", "unpin", visible)
	if unpin.Action != l2.GestureUndo || unpin.Undoes != pin.ID {
		t.Fatalf("unpin = %+v", unpin)
	}
	first, second := record("kyle", "pin", visible), record("sam", "pin", visible)
	out, err := call("sam", "unpin", visible)
	if err != nil {
		t.Fatal(err)
	}
	last := ledger()
	if len(last) < 2 || out != fmt.Sprintf("%d\n%d\n", last[len(last)-2].ID, last[len(last)-1].ID) || last[len(last)-2].Undoes != second.ID || last[len(last)-1].Undoes != first.ID {
		t.Fatalf("unpin all = %q, %+v", out, last)
	}
	pins, err := graph.Pins(ctx, "code:acme/api")
	if err != nil || len(pins) != 0 {
		t.Fatalf("pins after unpin = %+v, %v", pins, err)
	}
}

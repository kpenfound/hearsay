//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
)

// `hearsay eval` end to end over a scratch database: stance history, gestures
// and topic operations seeded at known times, and the metrics each window and
// scope reads from them, as text and as JSON, with nothing in either but
// counts, durations, rates and ids.
//
// Scope api: topic a is ratified by hand ten hours after its stance, c three
// hours after, b never; d was merged into a and the merge undone; b was split,
// and the split's topic s is a topic of its own. Scope web holds w, never
// ratified.
//
// The sessions, on bundle scope api: s1 is served a bundle at hour 1, follows
// its anchor, searches past it, and proceeds, judging topic a's conflict real.
// s2 is served one at hour 4, follows nothing, and asks, judging neither
// conflict. A bundle on api at hour 5 has no session, and s3's bundle at hour
// 6 is on web.
func TestEvalCommand(t *testing.T) {
	url, pool := deleteDatabase(t, "eval_cli_")
	ctx := t.Context()
	configPath := writeConfig(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"principals/p.yaml":   "- id: kyle\n  identities: [{source: github, native_id: u1}]\n",
		"authority/a.yaml":    "scope: \"*\"\nratified_by:\n  principals: [kyle]\n",
	})
	repo, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	hour := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	stamp := func(h int) string { return hour(h).Format(time.RFC3339) }
	graph := l2.New(pool)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	// secrets is every name and position seeded: none may be printed.
	var secrets []string
	open := func(scope, name string) l2.Topic {
		t.Helper()
		doc := "l1:github:open-" + scope + "-" + strings.ReplaceAll(name, " ", "-")
		topic := l2.Topic{ID: l2.TopicID(scope, doc, 0, name), Scope: scope, Name: name, ACL: public, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE l2_topics SET created_at = $1 WHERE id = $2`, hour(0), topic.ID); err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, name)
		return topic
	}
	// stance writes a position on a topic from a document of its own, as
	// written at hour h, and returns the stance and its document.
	stance := func(topic l2.Topic, position string, h int) (string, string) {
		t.Helper()
		doc := "l1:discord:" + strings.ReplaceAll(position, " ", "-")
		at := hour(h)
		st := l2.Stance{ID: l2.StanceID(topic.ID, doc, position, at, l2.TierInferred), TopicID: topic.ID, Position: position,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: public}
		stored, _, err := graph.AppendStance(ctx, st, at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE l2_stances SET created_at = $1 WHERE id = $2`, at, stored.ID); err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, position)
		return stored.ID, doc
	}
	ratify := func(doc string, h int) {
		t.Helper()
		g, _, err := l2.RecordGesture(ctx, pool, repo, l2.GestureRequest{
			Event: connector.EventID("discord", "reaction-"+strings.ReplaceAll(doc, ":", "-")), Principal: "kyle", Action: l2.GestureRatify, Documents: []string{doc},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE l2_gestures SET created_at = $1 WHERE id = $2`, hour(h), g.ID); err != nil {
			t.Fatal(err)
		}
	}
	operate := func(req l2.OperationRequest, h int) l2.Operation {
		t.Helper()
		req.Principal = "kyle"
		op, err := l2.Operate(ctx, pool, repo, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE l2_topic_operations SET created_at = $1 WHERE id = $2`, hour(h), op.ID); err != nil {
			t.Fatal(err)
		}
		return op
	}

	a, b, c, d := open("api", "the lock"), open("api", "the queue"), open("api", "the cache"), open("api", "the lock again")
	w := open("web", "the theme")
	_, aDoc := stance(a, "the queue takes the lock", 0)
	stance(b, "one queue per service", 2)
	b2, _ := stance(b, "one queue per team", 3)
	_, cDoc := stance(c, "drop the cache", 1)
	stance(d, "the engine takes the lock", 3)
	stance(w, "dark by default", 0)
	ratify(aDoc, 10)
	ratify(cDoc, 4)
	merge := operate(l2.OperationRequest{Kind: l2.OperationMerge, Into: a.ID, From: d.ID}, 5)
	operate(l2.OperationRequest{Kind: l2.OperationUndo, Undoes: merge.ID}, 6)
	split := operate(l2.OperationRequest{Kind: l2.OperationSplit, Topic: b.ID, Name: "per team", Stances: []string{b2}}, 7)
	secrets = append(secrets, "per team")
	s := split.Topics[1]

	events := l0.New(pool)
	appendEvent := func(ev connector.Event) {
		t.Helper()
		if _, err := events.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	kyle := connector.Identity{Source: connector.SelfSource, Kind: connector.IdentityUser, NativeID: "kyle"}
	// audit writes an audit record at hour h, as the API writes one.
	audit := func(id string, h int, record map[string]any) {
		t.Helper()
		native, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		appendEvent(connector.Event{Source: connector.SelfSource, NativeID: id, Kind: connector.KindAudit, Time: hour(h),
			Payload: connector.Payload{Artifact: id, Container: connector.Container{Kind: connector.ContainerWorkspace, NativeID: "api"},
				Author: &kyle, Native: native},
			ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: connector.SelfSource, NativeID: "kyle"}}})
	}
	sections := map[string]any{
		"scope.entities": map[string]any{"entities": []string{"code:api"}},
		"anchors":        map[string]any{"l1": []string{"l1:github:anchor"}},
		"stances":        map[string]any{"topics": []string{a.ID}, "l1": []string{aDoc}},
		"recent":         map[string]any{"l1": []string{"l1:github:recent"}},
		"open_questions": map[string]any{"l1": []string{"l1:github:question"}},
		"conflicts":      map[string]any{"topics": []string{a.ID, b.ID}},
	}
	bundle := func(id, session, scope string, h int) {
		t.Helper()
		record := map[string]any{"call": "get_bundle", "principal": "kyle", "scope": scope, "bundle": "sha256:" + id, "sections": sections}
		if session != "" {
			record["session"], record["session_source"] = session, "agents"
		}
		audit("bundle:"+id, h, record)
	}
	handleCall := func(id, session, call string, h int, fields map[string]any) {
		t.Helper()
		record := map[string]any{"call": call, "principal": "kyle", "session": session, "session_source": "agents"}
		for k, v := range fields {
			record[k] = v
		}
		audit("handle:"+id, h, record)
	}
	// nextAction posts a next action at hour h, as the agent source writes
	// one: its time is the session's start and its revision's is when it
	// happened.
	nextAction := func(session, next string, h int, native map[string]any) {
		t.Helper()
		native["session"], native["next"] = session, next
		raw, err := json.Marshal(native)
		if err != nil {
			t.Fatal(err)
		}
		agentID := connector.Identity{Source: "agents", Kind: connector.IdentityAgent, NativeID: "shed"}
		appendEvent(connector.Event{Source: "agents", NativeID: session + "@next:" + next, Kind: connector.KindNextAction, Time: hour(0),
			Payload: connector.Payload{Artifact: session, Container: connector.Container{Kind: "stream", NativeID: "shed"}, Author: &agentID,
				Revision: &connector.Revision{Token: "next:" + next, EditedAt: hour(h)}, Native: raw},
			ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: "agents", NativeID: "shed"}}})
	}
	bundle("1", "s1", "api", 1)
	handleCall("1", "s1", "get_l1", 2, map[string]any{"target_ids": []string{"l1:github:anchor"}})
	handleCall("2", "s1", "search", 2, map[string]any{"returned_ids": []string{"l1:github:elsewhere"}})
	nextAction("s1", "1", 3, map[string]any{"scope": "api", "action": "proceeded",
		"verdicts": []map[string]string{{"topic_id": a.ID, "verdict": "real"}}})
	bundle("2", "s2", "api", 4)
	nextAction("s2", "1", 4, map[string]any{"scope": "api", "action": "asked"})
	bundle("3", "", "api", 5)
	bundle("4", "s3", "web", 6)

	eval := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		if err := run(ctx, append([]string{"eval", "--database-url", url, "--config", configPath}, args...), &out, &stderr); err != nil {
			t.Fatalf("eval %v = %v\n%s", args, err, stderr.String())
		}
		for _, secret := range secrets {
			if strings.Contains(out.String(), secret) {
				t.Errorf("eval %v printed %q, which is a topic's name or a stance's position:\n%s", args, secret, out.String())
			}
		}
		return out.String()
	}

	t.Run("text", func(t *testing.T) {
		got := eval("--scope", "api", "--since", stamp(0), "--until", stamp(24))
		want := []string{
			"window " + stamp(0) + " to " + stamp(24),
			"scope api",
			"",
			"time to ratification",
			"topics 5",
			"ratified 2",
			"still unratified 3 60.0%",
			"median 3h0m0s",
			"p90 10h0m0s",
			"ratified on arrival 0",
			"",
			"topic merge and split rate",
			"topics opened 4",
			"merges 1 0.250 per topic 1 undone",
			"splits 1 0.250 per topic 0 undone",
			"",
			"drill-down rate per bundle section",
			"session-linked bundles 2",
			"excluded, no session 1",
			"scope.entities 0 of 2 0.0%",
			"anchors 1 of 2 50.0%",
			"stances 0 of 2 0.0%",
			"recent 0 of 2 0.0%",
			"open_questions 0 of 2 0.0%",
			"conflicts 0 of 2 0.0%",
			"looked beyond the bundle",
			"search 1 of 1 100.0%",
			"resolve 0 of 0 -",
			"",
			"conflict-flag precision",
			"flagged 4",
			"verdicts 1 1 real 0 spurious",
			"precision 100.0%",
			"coverage 25.0%",
			"verdicts on unflagged topics 0",
			"",
			"next actions",
			"after a bundle 2",
			"asked 1 50.0%",
			"proceeded 1 50.0%",
			"asserted 0 0.0%",
			"with no bundle before them 0",
		}
		if lines := table(got); strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Errorf("eval printed\n%s\nwant (spacing aside)\n%s", got, strings.Join(want, "\n"))
		}
	})

	t.Run("json", func(t *testing.T) {
		got := eval("--scope", "api", "--since", stamp(0), "--until", stamp(24), "--json")
		want := `{
  "since": "` + stamp(0) + `", "until": "` + stamp(24) + `", "scope": "api",
  "time_to_ratification": {
    "topics": 5, "ratified": 2, "unratified": 3, "unratified_share": 0.6,
    "median_seconds": 10800, "p90_seconds": 36000, "ratified_on_arrival": 0,
    "clocks": [
      {"topic": "` + a.ID + `", "scope": "api", "started": "` + stamp(0) + `", "ratified": "` + stamp(10) + `", "seconds": 36000},
      {"topic": "` + c.ID + `", "scope": "api", "started": "` + stamp(1) + `", "ratified": "` + stamp(4) + `", "seconds": 10800},
      {"topic": "` + b.ID + `", "scope": "api", "started": "` + stamp(2) + `", "ratified": null, "seconds": null},
      ` + clocksAt3(d.ID, s, stamp(3)) + `
    ]
  },
  "topic_operations": {
    "topics_opened": 4,
    "merges": {"count": 1, "undone": 1, "per_topic": 0.25},
    "splits": {"count": 1, "undone": 0, "per_topic": 0.25}
  },
  "drill_down": {
    "bundles": 2, "bundles_without_session": 1,
    "sections": [
      {"section": "scope.entities", "served": 2, "followed": 0, "share": 0},
      {"section": "anchors", "served": 2, "followed": 1, "share": 0.5},
      {"section": "stances", "served": 2, "followed": 0, "share": 0},
      {"section": "recent", "served": 2, "followed": 0, "share": 0},
      {"section": "open_questions", "served": 2, "followed": 0, "share": 0},
      {"section": "conflicts", "served": 2, "followed": 0, "share": 0}
    ],
    "looked_beyond": {
      "search": {"calls": 1, "beyond": 1, "share": 1},
      "resolve": {"calls": 0, "beyond": 0, "share": null}
    }
  },
  "conflict_flags": {
    "flagged": 4, "verdicts": 1, "real": 1, "spurious": 0,
    "precision": 1, "coverage": 0.25, "unflagged_verdicts": 0
  },
  "next_actions": {
    "actions": 2,
    "asked": {"count": 1, "share": 0.5},
    "proceeded": {"count": 1, "share": 0.5},
    "asserted": {"count": 0, "share": 0},
    "unattributed": 0
  }
}`
		if a, b := canonicalJSON(t, got), canonicalJSON(t, want); a != b {
			t.Errorf("eval --json =\n%s\nwant\n%s", a, b)
		}
	})

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			// a is ratified at hour 10, after the window; the undo at hour 6
			// is not before its end, so the merge is not undone yet, and the
			// split at hour 7 has not happened.
			name: "until hour 6",
			args: []string{"--scope", "api", "--since", stamp(0), "--until", stamp(6)},
			want: []string{"topics 5", "ratified 1", "still unratified 4 80.0%", "median 3h0m0s", "p90 3h0m0s",
				"merges 1 0.250 per topic 0 undone", "splits 0 0.000 per topic 0 undone",
				"session-linked bundles 2", "excluded, no session 1"},
		},
		{
			// s1's next action happens at hour 3, the window's end.
			name: "until hour 3",
			args: []string{"--scope", "api", "--until", stamp(3)},
			want: []string{"session-linked bundles 1", "excluded, no session 0", "anchors 1 of 1 100.0%", "search 1 of 1 100.0%",
				"flagged 2", "verdicts 0 0 real 0 spurious", "precision -", "coverage 0.0%", "after a bundle 0", "asked 0 -"},
		},
		{
			// Only the clocks that started at or after hour 2, and no topic
			// opened in the window to divide by.
			name: "since hour 2",
			args: []string{"--scope", "api", "--since", stamp(2), "--until", stamp(24)},
			// s1's bundle is before the window, so its handle calls follow
			// nothing in it and its next action has no bundle before it.
			want: []string{"topics 3", "ratified 0", "still unratified 3 100.0%", "median -", "p90 -", "topics opened 0",
				"merges 1 - per topic 1 undone", "splits 1 - per topic 0 undone",
				"session-linked bundles 1", "excluded, no session 1", "anchors 0 of 1 0.0%", "search 0 of 0 -",
				"flagged 2", "verdicts 0 0 real 0 spurious", "coverage 0.0%", "after a bundle 1", "asked 1 100.0%",
				"with no bundle before them 1"},
		},
		{
			name: "another scope",
			args: []string{"--scope", "web", "--until", stamp(24)},
			want: []string{"window the beginning to " + stamp(24), "scope web", "topics 1", "ratified 0", "still unratified 1 100.0%",
				"topics opened 1", "merges 0 0.000 per topic 0 undone", "splits 0 0.000 per topic 0 undone",
				"session-linked bundles 1", "excluded, no session 0", "anchors 0 of 1 0.0%", "after a bundle 0"},
		},
		{
			name: "every scope",
			args: []string{"--until", stamp(24)},
			want: []string{"scope every scope", "topics 6", "ratified 2", "still unratified 4 66.7%", "topics opened 5",
				"merges 1 0.200 per topic 1 undone", "splits 1 0.200 per topic 0 undone",
				"session-linked bundles 3", "excluded, no session 1", "anchors 1 of 3 33.3%", "flagged 6", "coverage 16.7%"},
		},
		{
			name: "a scope with nothing in it",
			args: []string{"--scope", "nowhere", "--until", stamp(24)},
			want: []string{"topics 0", "still unratified 0 -", "median -", "topics opened 0", "merges 0 - per topic 0 undone",
				"session-linked bundles 0", "excluded, no session 0", "anchors 0 of 0 -", "flagged 0", "after a bundle 0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := table(eval(tt.args...))
			for _, want := range tt.want {
				found := false
				for _, line := range lines {
					found = found || line == want
				}
				if !found {
					t.Errorf("no line %q in\n%s", want, strings.Join(lines, "\n"))
				}
			}
		})
	}
}

// clocksAt3 is the two clocks that started at hour 3, d's and the split's,
// in topic id order, which is how clocks that start together are ordered.
func clocksAt3(d, s, at string) string {
	first, second := d, s
	if s < d {
		first, second = s, d
	}
	clock := func(id string) string {
		return `{"topic": "` + id + `", "scope": "api", "started": "` + at + `", "ratified": null, "seconds": null}`
	}
	return clock(first) + ", " + clock(second)
}

func canonicalJSON(t *testing.T, doc string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, doc)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

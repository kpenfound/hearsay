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
				"merges 1 0.250 per topic 0 undone", "splits 0 0.000 per topic 0 undone"},
		},
		{
			// Only the clocks that started at or after hour 2, and no topic
			// opened in the window to divide by.
			name: "since hour 2",
			args: []string{"--scope", "api", "--since", stamp(2), "--until", stamp(24)},
			want: []string{"topics 3", "ratified 0", "still unratified 3 100.0%", "median -", "p90 -", "topics opened 0",
				"merges 1 - per topic 1 undone", "splits 1 - per topic 0 undone"},
		},
		{
			name: "another scope",
			args: []string{"--scope", "web", "--until", stamp(24)},
			want: []string{"window the beginning to " + stamp(24), "scope web", "topics 1", "ratified 0", "still unratified 1 100.0%",
				"topics opened 1", "merges 0 0.000 per topic 0 undone", "splits 0 0.000 per topic 0 undone"},
		},
		{
			name: "every scope",
			args: []string{"--until", stamp(24)},
			want: []string{"scope every scope", "topics 6", "ratified 2", "still unratified 4 66.7%", "topics opened 5",
				"merges 1 0.200 per topic 1 undone", "splits 1 0.200 per topic 0 undone"},
		},
		{
			name: "a scope with nothing in it",
			args: []string{"--scope", "nowhere", "--until", stamp(24)},
			want: []string{"topics 0", "still unratified 0 -", "median -", "topics opened 0", "merges 0 - per topic 0 undone"},
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

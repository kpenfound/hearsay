package eval_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/eval"
)

// ids is one section of a bundle audit, in the audit record's shape.
type ids struct {
	L1       []string `json:"l1,omitempty"`
	Topics   []string `json:"topics,omitempty"`
	Entities []string `json:"entities,omitempty"`
}

func audit(t *testing.T, record map[string]any) connector.Event {
	t.Helper()
	native, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return connector.Event{Source: connector.SelfSource, Kind: connector.KindAudit, Payload: connector.Payload{Native: native}}
}

// bundle is a bundle audit in session s (none where empty) on scope, serving
// sections.
func bundle(t *testing.T, s, scope string, sections map[string]ids) connector.Event {
	record := map[string]any{"call": "get_bundle", "scope": scope, "sections": sections}
	if s != "" {
		record["session"], record["session_source"] = s, "agents"
	}
	return audit(t, record)
}

// handle is a handle audit in session s.
func handle(t *testing.T, s, call string, fields map[string]any) connector.Event {
	record := map[string]any{"call": call, "session": s, "session_source": "agents"}
	for k, v := range fields {
		record[k] = v
	}
	return audit(t, record)
}

// next is a next action in session s, with verdicts as topic, verdict pairs.
func next(t *testing.T, s, scope, action string, verdicts ...string) connector.Event {
	t.Helper()
	native := map[string]any{"session": s, "next": "n", "scope": scope, "action": action}
	var vs []map[string]string
	for i := 0; i+1 < len(verdicts); i += 2 {
		vs = append(vs, map[string]string{"topic_id": verdicts[i], "verdict": verdicts[i+1]})
	}
	if vs != nil {
		native["verdicts"] = vs
	}
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	return connector.Event{Source: "agents", Kind: connector.KindNextAction, Payload: connector.Payload{Native: raw}}
}

func pct(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *f)
}

// summary is the report's figures on one line: sections as followed/served,
// looked beyond as beyond/calls, each with its share.
func summary(d eval.DrillDown, f eval.ConflictFlags, a eval.NextActions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "bundles=%d nosession=%d", d.Bundles, d.WithoutSession)
	for _, s := range d.Sections {
		if s.Served > 0 || s.Share != nil {
			fmt.Fprintf(&b, " %s=%d/%d(%s)", s.Section, s.Followed, s.Served, pct(s.Share))
		}
	}
	fmt.Fprintf(&b, " search=%d/%d(%s) resolve=%d/%d(%s)", d.LookedBeyond.Search.Beyond, d.LookedBeyond.Search.Calls, pct(d.LookedBeyond.Search.Share),
		d.LookedBeyond.Resolve.Beyond, d.LookedBeyond.Resolve.Calls, pct(d.LookedBeyond.Resolve.Share))
	fmt.Fprintf(&b, " | flagged=%d verdicts=%d real=%d spurious=%d precision=%s coverage=%s unflagged=%d",
		f.Flagged, f.Verdicts, f.Real, f.Spurious, pct(f.Precision), pct(f.Coverage), f.Unflagged)
	fmt.Fprintf(&b, " | actions=%d asked=%d(%s) proceeded=%d(%s) asserted=%d(%s) unattributed=%d", a.Actions,
		a.Asked.Count, pct(a.Asked.Share), a.Proceeded.Count, pct(a.Proceeded.Share), a.Asserted.Count, pct(a.Asserted.Share), a.Unattributed)
	return b.String()
}

func TestSummarizeSessions(t *testing.T) {
	// full serves one id in every section.
	full := map[string]ids{
		"scope.entities": {Entities: []string{"code:api"}, L1: []string{"l1:readme"}},
		"anchors":        {L1: []string{"l1:anchor"}},
		"stances":        {Topics: []string{"topic:lock"}, L1: []string{"l1:evidence"}},
		"recent":         {L1: []string{"l1:recent"}},
		"open_questions": {L1: []string{"l1:question"}},
		"conflicts":      {Topics: []string{"topic:lock", "topic:queue"}},
	}
	anchorOnly := map[string]ids{"anchors": {L1: []string{"l1:other"}}, "conflicts": {}}
	const nothing = " search=0/0(-) resolve=0/0(-) | flagged=0 verdicts=0 real=0 spurious=0 precision=- coverage=- unflagged=0" +
		" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0"
	tests := []struct {
		name   string
		scope  string
		events func(t *testing.T) []connector.Event
		want   string
	}{
		{
			name:   "nothing",
			events: func(*testing.T) []connector.Event { return nil },
			want:   "bundles=0 nosession=0" + nothing,
		},
		{
			name: "a bundle with no follow-up",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{bundle(t, "s1", "api", full)}
			},
			want: "bundles=1 nosession=0 scope.entities=0/1(0.00) anchors=0/1(0.00) stances=0/1(0.00) recent=0/1(0.00)" +
				" open_questions=0/1(0.00) conflicts=0/1(0.00) search=0/0(-) resolve=0/0(-)" +
				" | flagged=2 verdicts=0 real=0 spurious=0 precision=- coverage=0.00 unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
		{
			name: "a bundle without a session is excluded and counted",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{bundle(t, "", "api", full), handle(t, "", "get_l1", map[string]any{"target_ids": []string{"l1:anchor"}})}
			},
			want: "bundles=0 nosession=1" + nothing,
		},
		{
			name: "each handle follows the section that served its id",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					bundle(t, "s1", "api", full),
					handle(t, "s1", "get_l1", map[string]any{"target_ids": []string{"l1:anchor"}}),
					handle(t, "s1", "stance_history", map[string]any{"target_ids": []string{"topic:queue"}, "returned_ids": []string{"topic:queue"}}),
					handle(t, "s1", "search", map[string]any{"scope": "code:api", "returned_ids": []string{"l1:recent"}}),
				}
			},
			// The search within code:api follows scope.entities, and
			// returned an id the bundle served, so it did not look beyond.
			want: "bundles=1 nosession=0 scope.entities=1/1(1.00) anchors=1/1(1.00) stances=0/1(0.00) recent=0/1(0.00)" +
				" open_questions=0/1(0.00) conflicts=1/1(1.00) search=0/1(0.00) resolve=0/0(-)" +
				" | flagged=2 verdicts=0 real=0 spurious=0 precision=- coverage=0.00 unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
		{
			name: "a returned id is not a follow, and a refused call follows nothing",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					bundle(t, "s1", "api", full),
					handle(t, "s1", "resolve", map[string]any{"returned_ids": []string{"code:api"}}),
					handle(t, "s1", "get_l1", map[string]any{"status": 404}),
					handle(t, "s1", "search", map[string]any{"status": 403}),
				}
			},
			want: "bundles=1 nosession=0 scope.entities=0/1(0.00) anchors=0/1(0.00) stances=0/1(0.00) recent=0/1(0.00)" +
				" open_questions=0/1(0.00) conflicts=0/1(0.00) search=0/0(-) resolve=0/1(0.00)" +
				" | flagged=2 verdicts=0 real=0 spurious=0 precision=- coverage=0.00 unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
		{
			name: "searches and resolves that return nothing the bundle served look beyond it",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					handle(t, "s1", "search", map[string]any{"returned_ids": []string{"l1:elsewhere"}}),
					bundle(t, "s1", "api", anchorOnly),
					handle(t, "s1", "search", map[string]any{"returned_ids": []string{"l1:elsewhere", "l1:other"}}),
					handle(t, "s1", "search", map[string]any{"returned_ids": []string{"l1:elsewhere"}}),
					handle(t, "s1", "search", map[string]any{}),
					handle(t, "s1", "resolve", map[string]any{"returned_ids": []string{"code:web"}}),
					handle(t, "s2", "search", map[string]any{"returned_ids": []string{"l1:elsewhere"}}),
				}
			},
			// The first search came before any bundle and the last in
			// another session: neither is counted.
			want: "bundles=1 nosession=0 anchors=0/1(0.00) search=2/3(0.67) resolve=1/1(1.00)" +
				" | flagged=0 verdicts=0 real=0 spurious=0 precision=- coverage=- unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
		{
			name: "a handle call after the next bundle on the scope follows only the new one",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					bundle(t, "s1", "api", full),
					bundle(t, "s1", "api", anchorOnly),
					handle(t, "s1", "get_l1", map[string]any{"target_ids": []string{"l1:anchor"}}),
					handle(t, "s1", "get_l1", map[string]any{"target_ids": []string{"l1:other"}}),
				}
			},
			want: "bundles=2 nosession=0 scope.entities=0/1(0.00) anchors=1/2(0.50) stances=0/1(0.00) recent=0/1(0.00)" +
				" open_questions=0/1(0.00) conflicts=0/1(0.00) search=0/0(-) resolve=0/0(-)" +
				" | flagged=2 verdicts=0 real=0 spurious=0 precision=- coverage=0.00 unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
		{
			name: "a bundle on another scope, or in another session, does not end one",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					bundle(t, "s1", "api", full),
					bundle(t, "s1", "web", anchorOnly),
					bundle(t, "s2", "api", anchorOnly),
					handle(t, "s1", "get_l1", map[string]any{"target_ids": []string{"l1:recent"}}),
					handle(t, "s2", "get_l1", map[string]any{"target_ids": []string{"l1:question"}}),
				}
			},
			want: "bundles=3 nosession=0 scope.entities=0/1(0.00) anchors=0/3(0.00) stances=0/1(0.00) recent=1/1(1.00)" +
				" open_questions=0/1(0.00) conflicts=0/1(0.00) search=0/0(-) resolve=0/0(-)" +
				" | flagged=2 verdicts=0 real=0 spurious=0 precision=- coverage=0.00 unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
		{
			name: "verdicts on flagged conflicts, the later one winning",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					next(t, "s1", "api", "asked"),
					bundle(t, "s1", "api", full),
					next(t, "s1", "api", "proceeded", "topic:lock", "spurious", "topic:cache", "real"),
					next(t, "s1", "api", "asserted", "topic:lock", "real"),
					next(t, "s1", "web", "asked"),
					next(t, "s2", "api", "asked", "topic:queue", "real"),
					bundle(t, "s1", "api", full),
					next(t, "s1", "api", "asked", "topic:queue", "spurious"),
				}
			},
			// Four conflicts flagged, two bundles of two; the first bundle's
			// lock is real (its later verdict), the second's queue spurious.
			// The cache was never flagged. Three actions have no bundle before
			// them on their scope in their session.
			want: "bundles=2 nosession=0 scope.entities=0/2(0.00) anchors=0/2(0.00) stances=0/2(0.00) recent=0/2(0.00)" +
				" open_questions=0/2(0.00) conflicts=0/2(0.00) search=0/0(-) resolve=0/0(-)" +
				" | flagged=4 verdicts=2 real=1 spurious=1 precision=0.50 coverage=0.50 unflagged=1" +
				" | actions=3 asked=1(0.33) proceeded=1(0.33) asserted=1(0.33) unattributed=3",
		},
		{
			name:  "a scope keeps its own bundles and next actions",
			scope: "web",
			events: func(t *testing.T) []connector.Event {
				return []connector.Event{
					bundle(t, "s1", "api", full),
					bundle(t, "", "api", full),
					bundle(t, "", "web", anchorOnly),
					bundle(t, "s1", "web", anchorOnly),
					handle(t, "s1", "get_l1", map[string]any{"target_ids": []string{"l1:anchor"}}),
					handle(t, "s1", "get_l1", map[string]any{"target_ids": []string{"l1:other"}}),
					next(t, "s1", "api", "asked", "topic:lock", "real"),
					next(t, "s1", "web", "proceeded"),
				}
			},
			want: "bundles=1 nosession=1 anchors=1/1(1.00) search=0/0(-) resolve=0/0(-)" +
				" | flagged=0 verdicts=0 real=0 spurious=0 precision=- coverage=- unflagged=0" +
				" | actions=1 asked=0(0.00) proceeded=1(1.00) asserted=0(0.00) unattributed=0",
		},
		{
			name: "a redacted audit or next action is skipped",
			events: func(t *testing.T) []connector.Event {
				redacted := func(kind connector.Kind, source string) connector.Event {
					return connector.Event{Source: source, Kind: kind}
				}
				return []connector.Event{
					redacted(connector.KindAudit, connector.SelfSource),
					bundle(t, "s1", "api", anchorOnly),
					redacted(connector.KindAudit, connector.SelfSource),
					redacted(connector.KindNextAction, "agents"),
				}
			},
			want: "bundles=1 nosession=0 anchors=0/1(0.00) search=0/0(-) resolve=0/0(-)" +
				" | flagged=0 verdicts=0 real=0 spurious=0 precision=- coverage=- unflagged=0" +
				" | actions=0 asked=0(-) proceeded=0(-) asserted=0(-) unattributed=0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drill, flags, actions := eval.SummarizeSessions(tt.events(t), tt.scope)
			if got := summary(drill, flags, actions); got != tt.want {
				t.Errorf("SummarizeSessions =\n%s\nwant\n%s", got, tt.want)
			}
			if len(drill.Sections) != 6 {
				t.Errorf("sections = %+v, want all six", drill.Sections)
			}
		})
	}
}

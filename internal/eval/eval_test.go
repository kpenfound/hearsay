package eval_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/l2"
)

var day0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func at(hours int) time.Time { return day0.Add(time.Duration(hours) * time.Hour) }

func secs(hours int) *float64 {
	s := float64(hours) * 3600
	return &s
}

func share(f float64) *float64 { return &f }

func ptrEqual(a, b *float64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func show(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestSummarizeRatifications(t *testing.T) {
	// A clock started at hour s and stopped at hour r; r < 0 never stopped.
	clock := func(topic string, s, r int) l2.Ratification {
		rat := l2.Ratification{Topic: topic, Scope: "api", Stood: at(s), Unratified: at(s)}
		if r >= 0 {
			rat.Ratified = at(r)
		}
		return rat
	}
	arrived := func(topic string, h int) l2.Ratification {
		return l2.Ratification{Topic: topic, Scope: "api", Stood: at(h)}
	}
	tests := []struct {
		name         string
		rs           []l2.Ratification
		since, until time.Time
		topics       int
		ratified     int
		unratified   int
		share        *float64
		median, p90  *float64
		arrivals     int
		clocks       []string
	}{
		{
			name: "nothing", until: at(100),
			clocks: []string{},
		},
		{
			name: "one ratified, one still open at until",
			rs:   []l2.Ratification{clock("a", 0, 10), clock("b", 5, -1)}, until: at(100),
			topics: 2, ratified: 1, unratified: 1, share: share(0.5), median: secs(10), p90: secs(10),
			clocks: []string{"a", "b"},
		},
		{
			name: "a ratification at or after until is still open",
			rs:   []l2.Ratification{clock("a", 0, 100)}, until: at(100),
			topics: 1, unratified: 1, share: share(1),
			clocks: []string{"a"},
		},
		{
			name: "nearest-rank median and p90 over ten",
			rs: []l2.Ratification{
				clock("a", 0, 1), clock("b", 0, 2), clock("c", 0, 3), clock("d", 0, 4), clock("e", 0, 5),
				clock("f", 0, 6), clock("g", 0, 7), clock("h", 0, 8), clock("i", 0, 9), clock("j", 0, 100),
			},
			until:  at(200),
			topics: 10, ratified: 10, share: share(0), median: secs(5), p90: secs(9),
			clocks: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
		},
		{
			name:  "a clock counts in the window it started in",
			rs:    []l2.Ratification{clock("before", 0, 30), clock("edge", 10, 12), clock("inside", 20, -1), clock("after", 50, -1)},
			since: at(10), until: at(50),
			topics: 2, ratified: 1, unratified: 1, share: share(0.5), median: secs(2), p90: secs(2),
			clocks: []string{"edge", "inside"},
		},
		{
			name:  "ratified on arrival counts where it first stood in the window, and starts no clock",
			rs:    []l2.Ratification{arrived("early", 0), arrived("in", 15), clock("late", 12, 13)},
			since: at(10), until: at(50),
			topics: 1, ratified: 1, share: share(0), median: secs(1), p90: secs(1), arrivals: 1,
			clocks: []string{"late"},
		},
		{
			name: "a topic that never stood before until counts nowhere",
			rs:   []l2.Ratification{{Topic: "empty", Scope: "api"}}, until: at(50),
			clocks: []string{},
		},
		{
			name: "clocks are ordered by start, then topic",
			rs:   []l2.Ratification{clock("z", 3, -1), clock("b", 1, -1), clock("a", 3, -1)}, until: at(50),
			topics: 3, unratified: 3, share: share(1),
			clocks: []string{"b", "a", "z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.SummarizeRatifications(tt.rs, tt.since, tt.until)
			if got.Topics != tt.topics || got.Ratified != tt.ratified || got.Unratified != tt.unratified || got.RatifiedOnArrival != tt.arrivals {
				t.Errorf("counts = %d topics, %d ratified, %d unratified, %d on arrival; want %d, %d, %d, %d",
					got.Topics, got.Ratified, got.Unratified, got.RatifiedOnArrival, tt.topics, tt.ratified, tt.unratified, tt.arrivals)
			}
			if !ptrEqual(got.UnratifiedShare, tt.share) || !ptrEqual(got.MedianSeconds, tt.median) || !ptrEqual(got.P90Seconds, tt.p90) {
				t.Errorf("share, median, p90 = %v, %v, %v; want %v, %v, %v",
					show(got.UnratifiedShare), show(got.MedianSeconds), show(got.P90Seconds), show(tt.share), show(tt.median), show(tt.p90))
			}
			topics := []string{}
			for _, c := range got.Clocks {
				topics = append(topics, c.Topic)
				if (c.Ratified == nil) != (c.Seconds == nil) {
					t.Errorf("clock %s has ratified %v and seconds %v", c.Topic, c.Ratified, show(c.Seconds))
				}
			}
			if len(topics) != len(tt.clocks) {
				t.Fatalf("clocks = %v, want %v", topics, tt.clocks)
			}
			for i := range topics {
				if topics[i] != tt.clocks[i] {
					t.Fatalf("clocks = %v, want %v", topics, tt.clocks)
				}
			}
		})
	}
}

func TestSummarizeOperations(t *testing.T) {
	op := func(id int64, kind l2.OperationKind, h int, undoneBy int64) l2.Operation {
		return l2.Operation{ID: id, Kind: kind, Scope: "api", At: at(h), UndoneBy: undoneBy}
	}
	tests := []struct {
		name           string
		ops            []l2.Operation
		opened         int
		since          time.Time
		merges, splits eval.Count
	}{
		{
			name: "nothing opened, nothing to divide by",
		},
		{
			name:   "an undone merge counts, and is reported beside",
			ops:    []l2.Operation{op(1, l2.OperationMerge, 1, 3), op(2, l2.OperationSplit, 2, 0), op(3, l2.OperationUndo, 3, 0)},
			opened: 4,
			merges: eval.Count{Count: 1, Undone: 1, PerTopic: share(0.25)},
			splits: eval.Count{Count: 1, PerTopic: share(0.25)},
		},
		{
			name:   "an undo not among the operations before until has not happened yet",
			ops:    []l2.Operation{op(1, l2.OperationMerge, 1, 7)},
			opened: 2,
			merges: eval.Count{Count: 1, PerTopic: share(0.5)},
			splits: eval.Count{PerTopic: share(0)},
		},
		{
			name:   "operations before since are left out, their undos inside do not count as operations",
			ops:    []l2.Operation{op(1, l2.OperationSplit, 1, 2), op(2, l2.OperationUndo, 12, 0), op(3, l2.OperationSplit, 13, 0)},
			opened: 1, since: at(10),
			merges: eval.Count{PerTopic: share(0)},
			splits: eval.Count{Count: 1, PerTopic: share(1)},
		},
		{
			name:   "operations and no topic opened have no rate",
			ops:    []l2.Operation{op(1, l2.OperationMerge, 1, 0)},
			merges: eval.Count{Count: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.SummarizeOperations(tt.ops, tt.opened, tt.since)
			if got.TopicsOpened != tt.opened {
				t.Errorf("topics opened = %d, want %d", got.TopicsOpened, tt.opened)
			}
			for _, c := range []struct {
				kind      string
				got, want eval.Count
			}{{"merges", got.Merges, tt.merges}, {"splits", got.Splits, tt.splits}} {
				if c.got.Count != c.want.Count || c.got.Undone != c.want.Undone || !ptrEqual(c.got.PerTopic, c.want.PerTopic) {
					t.Errorf("%s = %d, %d undone, %v per topic; want %d, %d, %v", c.kind,
						c.got.Count, c.got.Undone, show(c.got.PerTopic), c.want.Count, c.want.Undone, show(c.want.PerTopic))
				}
			}
		})
	}
}

// The text and the JSON are the two outputs scripts and people read; both are
// pinned whole, so a renamed field or a changed line is a failing test.
func TestReportOutputs(t *testing.T) {
	since, ratified := at(0), at(36)
	report := eval.Report{
		Since: &since, Until: at(100), Scope: "api",
		Ratification: eval.Ratification{
			Topics: 2, Ratified: 1, Unratified: 1, UnratifiedShare: share(0.5),
			MedianSeconds: secs(36), P90Seconds: secs(36), RatifiedOnArrival: 1,
			Clocks: []eval.Clock{
				{Topic: "topic:a", Scope: "api", Started: at(0), Ratified: &ratified, Seconds: secs(36)},
				{Topic: "topic:b", Scope: "api", Started: at(2)},
			},
		},
		Operations: eval.Operations{
			TopicsOpened: 4,
			Merges:       eval.Count{Count: 1, Undone: 1, PerTopic: share(0.25)},
			Splits:       eval.Count{PerTopic: share(0)},
		},
	}
	var text bytes.Buffer
	if err := report.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	wantText := `window  2026-09-01T00:00:00Z to 2026-09-05T04:00:00Z
scope   api

time to ratification
  topics               2
  ratified             1
  still unratified     1      50.0%
  median               36h0m0s
  p90                  36h0m0s
  ratified on arrival  1

topic merge and split rate
  topics opened        4
  merges               1      0.250 per topic  1 undone
  splits               0      0.000 per topic  0 undone
`
	if text.String() != wantText {
		t.Errorf("text =\n%s\nwant\n%s", text.String(), wantText)
	}

	empty := eval.Report{Until: at(100), Ratification: eval.Ratification{Clocks: []eval.Clock{}}}
	text.Reset()
	if err := empty.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	wantEmpty := `window  the beginning to 2026-09-05T04:00:00Z
scope   every scope

time to ratification
  topics               0
  ratified             0
  still unratified     0  -
  median               -
  p90                  -
  ratified on arrival  0

topic merge and split rate
  topics opened        0
  merges               0  - per topic  0 undone
  splits               0  - per topic  0 undone
`
	if text.String() != wantEmpty {
		t.Errorf("text of an empty report =\n%s\nwant\n%s", text.String(), wantEmpty)
	}

	var out bytes.Buffer
	if err := report.WriteJSON(&out); err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the JSON does not parse: %v\n%s", err, out.String())
	}
	wantJSON := `{
  "since": "2026-09-01T00:00:00Z",
  "until": "2026-09-05T04:00:00Z",
  "scope": "api",
  "time_to_ratification": {
    "topics": 2, "ratified": 1, "unratified": 1, "unratified_share": 0.5,
    "median_seconds": 129600, "p90_seconds": 129600, "ratified_on_arrival": 1,
    "clocks": [
      {"topic": "topic:a", "scope": "api", "started": "2026-09-01T00:00:00Z", "ratified": "2026-09-02T12:00:00Z", "seconds": 129600},
      {"topic": "topic:b", "scope": "api", "started": "2026-09-01T02:00:00Z", "ratified": null, "seconds": null}
    ]
  },
  "topic_operations": {
    "topics_opened": 4,
    "merges": {"count": 1, "undone": 1, "per_topic": 0.25},
    "splits": {"count": 0, "undone": 0, "per_topic": 0}
  }
}`
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if a, b := canonical(t, got), canonical(t, want); a != b {
		t.Errorf("JSON =\n%s\nwant\n%s", a, b)
	}

	out.Reset()
	if err := empty.WriteJSON(&out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantEmptyJSON := `{"since": null, "until": "2026-09-05T04:00:00Z", "scope": "",
  "time_to_ratification": {"topics": 0, "ratified": 0, "unratified": 0, "unratified_share": null,
    "median_seconds": null, "p90_seconds": null, "ratified_on_arrival": 0, "clocks": []},
  "topic_operations": {"topics_opened": 0,
    "merges": {"count": 0, "undone": 0, "per_topic": null}, "splits": {"count": 0, "undone": 0, "per_topic": null}}}`
	if err := json.Unmarshal([]byte(wantEmptyJSON), &want); err != nil {
		t.Fatal(err)
	}
	if a, b := canonical(t, got), canonical(t, want); a != b {
		t.Errorf("JSON of an empty report =\n%s\nwant\n%s", a, b)
	}
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

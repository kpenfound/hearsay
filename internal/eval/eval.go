// Package eval computes the evaluation metrics (docs/design.md#evaluation):
// whether distillation is trustworthy rather than just confident.
//
// A [Report] holds one section per metric. Time to ratification and the topic
// merge and split rate are read from the L2 ledgers — stances, gestures and
// topic operations — and need nothing else. The drill-down rate per bundle
// section, conflict-flag precision and the consumer's next actions are read
// from L0: the session-linked bundle and handle audits the API writes, and the
// `next_action` events agents post. Every section is aggregates: counts,
// durations, rates, and scope and topic ids. Nothing here returns an L0
// payload, L1 text, a stance's position or a topic's name, and of an audit or
// next action it decodes the ids and call names alone.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
)

// Options are what a report is computed over.
type Options struct {
	// Since and Until bound the window: at or after Since, and before Until.
	// A zero Since is the beginning. Until is required: what is still open at
	// the end of the window is measured at it.
	Since, Until time.Time
	// Scope is the serial key the topics and operations are in
	// (`l2_topics.scope`), and the scope the bundles and next actions name;
	// empty is every scope.
	Scope string
}

// Report is the metrics over one window. Its JSON is the stable output of
// `hearsay eval --json`: a field is added, never renamed or removed.
type Report struct {
	// Since is null where the window starts at the beginning.
	Since *time.Time `json:"since"`
	Until time.Time  `json:"until"`
	// Scope is empty for every scope.
	Scope        string        `json:"scope"`
	Ratification Ratification  `json:"time_to_ratification"`
	Operations   Operations    `json:"topic_operations"`
	DrillDown    DrillDown     `json:"drill_down"`
	Conflicts    ConflictFlags `json:"conflict_flags"`
	NextActions  NextActions   `json:"next_actions"`
}

// Ratification is time to ratification: for each topic whose clock started in
// the window — it first stood at a stance not served as ratified then — how
// long until it first stood ratified ([l2.Store.Ratifications]).
type Ratification struct {
	// Topics is how many topics' clocks started in the window.
	Topics int `json:"topics"`
	// Ratified is how many of them stood ratified before the window's end, and
	// Unratified how many had not.
	Ratified   int `json:"ratified"`
	Unratified int `json:"unratified"`
	// UnratifiedShare is Unratified over Topics; null where Topics is zero.
	UnratifiedShare *float64 `json:"unratified_share"`
	// MedianSeconds and P90Seconds are over the ratified topics, by nearest
	// rank; null where none was ratified.
	MedianSeconds *float64 `json:"median_seconds"`
	P90Seconds    *float64 `json:"p90_seconds"`
	// RatifiedOnArrival is how many topics first stood in the window, already
	// ratified, and never stood otherwise before its end. They started no
	// clock and are not in Topics.
	RatifiedOnArrival int `json:"ratified_on_arrival"`
	// Clocks are the topics counted in Topics, by when their clock started.
	Clocks []Clock `json:"clocks"`
}

// Clock is one topic's time to ratification.
type Clock struct {
	Topic string `json:"topic"`
	Scope string `json:"scope"`
	// Started is when the topic first stood unratified, and Ratified when it
	// first stood ratified after that; null where it had not by the window's
	// end, as is Seconds.
	Started  time.Time  `json:"started"`
	Ratified *time.Time `json:"ratified"`
	Seconds  *float64   `json:"seconds"`
}

// Operations is the topic merge and split rate: the merges and splits people
// recorded in the window, per topic the assertion worker opened in it.
type Operations struct {
	TopicsOpened int   `json:"topics_opened"`
	Merges       Count `json:"merges"`
	Splits       Count `json:"splits"`
}

// Count is one kind of operation in the window.
type Count struct {
	// Count is every one recorded in the window, undone or not.
	Count int `json:"count"`
	// Undone is how many of them an undo recorded before the window's end
	// reversed. They are in Count too: an undo is reported, not netted out.
	Undone int `json:"undone"`
	// PerTopic is Count over the topics opened in the window; null where none
	// was.
	PerTopic *float64 `json:"per_topic"`
}

// Compute reads the ledgers and the session trails and computes the report.
// Standing is computed under authority, the configuration's.
func Compute(ctx context.Context, q l2.Querier, authority config.Authority, opts Options) (Report, error) {
	if opts.Until.IsZero() {
		return Report{}, errors.New("an evaluation window needs an end")
	}
	if !opts.Since.IsZero() && !opts.Since.Before(opts.Until) {
		return Report{}, fmt.Errorf("the evaluation window starts at %s, not before its end at %s",
			opts.Since.Format(time.RFC3339), opts.Until.Format(time.RFC3339))
	}
	store := l2.New(q)
	report := Report{Until: opts.Until.UTC(), Scope: opts.Scope}
	if !opts.Since.IsZero() {
		since := opts.Since.UTC()
		report.Since = &since
	}
	ratifications, err := store.Ratifications(ctx, authority, opts.Scope, opts.Until)
	if err != nil {
		return Report{}, err
	}
	report.Ratification = SummarizeRatifications(ratifications, opts.Since, opts.Until)
	opened, err := store.TopicsOpened(ctx, opts.Scope, opts.Since, opts.Until)
	if err != nil {
		return Report{}, err
	}
	// Every operation before the end, so an undo recorded after the window
	// started but before it ended is found for an operation inside it.
	ops, err := store.Operations(ctx, l2.OperationFilter{Scope: opts.Scope, Until: opts.Until})
	if err != nil {
		return Report{}, err
	}
	report.Operations = SummarizeOperations(ops, opened, opts.Since)
	trail, err := l0.New(q).Timeline(ctx, []connector.Kind{connector.KindAudit, connector.KindNextAction}, opts.Since, opts.Until)
	if err != nil {
		return Report{}, err
	}
	report.DrillDown, report.Conflicts, report.NextActions = SummarizeSessions(trail, opts.Scope)
	return report, nil
}

// SummarizeRatifications is time to ratification over the topics whose clock
// started at or after since and before until. The ratifications are what
// [l2.Store.Ratifications] replayed up to until.
func SummarizeRatifications(rs []l2.Ratification, since, until time.Time) Ratification {
	in := func(at time.Time) bool { return !at.IsZero() && !at.Before(since) && at.Before(until) }
	out := Ratification{Clocks: []Clock{}}
	var took []time.Duration
	for _, r := range rs {
		if r.Unratified.IsZero() {
			if in(r.Stood) {
				out.RatifiedOnArrival++
			}
			continue
		}
		if !in(r.Unratified) {
			continue
		}
		c := Clock{Topic: r.Topic, Scope: r.Scope, Started: r.Unratified.UTC()}
		out.Topics++
		if r.Ratified.IsZero() || !r.Ratified.Before(until) {
			out.Unratified++
		} else {
			out.Ratified++
			at, d := r.Ratified.UTC(), r.Ratified.Sub(r.Unratified)
			c.Ratified, c.Seconds = &at, seconds(d)
			took = append(took, d)
		}
		out.Clocks = append(out.Clocks, c)
	}
	slices.SortStableFunc(out.Clocks, func(a, b Clock) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.Topic, b.Topic)
	})
	if out.Topics > 0 {
		out.UnratifiedShare = ratio(out.Unratified, out.Topics)
	}
	if len(took) > 0 {
		slices.Sort(took)
		out.MedianSeconds, out.P90Seconds = seconds(nearestRank(took, 0.5)), seconds(nearestRank(took, 0.9))
	}
	return out
}

// SummarizeOperations is the merge and split rate over the operations
// recorded at or after since, of ops: every operation in the ledger before the
// window's end, with the undos among them. opened is the topics opened in the
// window.
func SummarizeOperations(ops []l2.Operation, opened int, since time.Time) Operations {
	undo := map[int64]bool{}
	for _, op := range ops {
		if op.Kind == l2.OperationUndo {
			undo[op.ID] = true
		}
	}
	out := Operations{TopicsOpened: opened}
	for _, op := range ops {
		if op.At.Before(since) {
			continue
		}
		var c *Count
		switch op.Kind {
		case l2.OperationMerge:
			c = &out.Merges
		case l2.OperationSplit:
			c = &out.Splits
		default:
			continue
		}
		c.Count++
		if op.UndoneBy != 0 && undo[op.UndoneBy] {
			c.Undone++
		}
	}
	if opened > 0 {
		out.Merges.PerTopic, out.Splits.PerTopic = ratio(out.Merges.Count, opened), ratio(out.Splits.Count, opened)
	}
	return out
}

// nearestRank is the p-th percentile of sorted durations: the smallest one at
// least p of them are no greater than.
func nearestRank(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func seconds(d time.Duration) *float64 {
	s := d.Seconds()
	return &s
}

func ratio(n, of int) *float64 {
	r := float64(n) / float64(of)
	return &r
}

// WriteJSON writes the report as `hearsay eval --json` prints it.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the report for a person: the window, then each metric's
// aggregates. The topic ids behind time to ratification are in the JSON.
func (r Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	since := "the beginning"
	if r.Since != nil {
		since = r.Since.Format(time.RFC3339)
	}
	scope := r.Scope
	if scope == "" {
		scope = "every scope"
	}
	fmt.Fprintf(tw, "window\t%s to %s\n", since, r.Until.Format(time.RFC3339))
	fmt.Fprintf(tw, "scope\t%s\n", scope)
	fmt.Fprintln(tw)

	rat := r.Ratification
	fmt.Fprintln(tw, "time to ratification")
	fmt.Fprintf(tw, "  topics\t%d\n", rat.Topics)
	fmt.Fprintf(tw, "  ratified\t%d\n", rat.Ratified)
	fmt.Fprintf(tw, "  still unratified\t%d\t%s\n", rat.Unratified, percent(rat.UnratifiedShare))
	fmt.Fprintf(tw, "  median\t%s\n", duration(rat.MedianSeconds))
	fmt.Fprintf(tw, "  p90\t%s\n", duration(rat.P90Seconds))
	fmt.Fprintf(tw, "  ratified on arrival\t%d\n", rat.RatifiedOnArrival)
	fmt.Fprintln(tw)

	ops := r.Operations
	fmt.Fprintln(tw, "topic merge and split rate")
	fmt.Fprintf(tw, "  topics opened\t%d\n", ops.TopicsOpened)
	fmt.Fprintf(tw, "  merges\t%d\t%s per topic\t%d undone\n", ops.Merges.Count, rate(ops.Merges.PerTopic), ops.Merges.Undone)
	fmt.Fprintf(tw, "  splits\t%d\t%s per topic\t%d undone\n", ops.Splits.Count, rate(ops.Splits.PerTopic), ops.Splits.Undone)
	fmt.Fprintln(tw)

	drill := r.DrillDown
	fmt.Fprintln(tw, "drill-down rate per bundle section")
	fmt.Fprintf(tw, "  session-linked bundles\t%d\n", drill.Bundles)
	fmt.Fprintf(tw, "  excluded, no session\t%d\n", drill.WithoutSession)
	for _, sec := range drill.Sections {
		fmt.Fprintf(tw, "  %s\t%d of %d\t%s\n", sec.Section, sec.Followed, sec.Served, percent(sec.Share))
	}
	fmt.Fprintln(tw, "  looked beyond the bundle")
	for _, c := range []struct {
		call string
		b    Beyond
	}{{"search", drill.LookedBeyond.Search}, {"resolve", drill.LookedBeyond.Resolve}} {
		fmt.Fprintf(tw, "    %s\t%d of %d\t%s\n", c.call, c.b.Beyond, c.b.Calls, percent(c.b.Share))
	}
	fmt.Fprintln(tw)

	flags := r.Conflicts
	fmt.Fprintln(tw, "conflict-flag precision")
	fmt.Fprintf(tw, "  flagged\t%d\n", flags.Flagged)
	fmt.Fprintf(tw, "  verdicts\t%d\t%d real\t%d spurious\n", flags.Verdicts, flags.Real, flags.Spurious)
	fmt.Fprintf(tw, "  precision\t%s\n", percent(flags.Precision))
	fmt.Fprintf(tw, "  coverage\t%s\n", percent(flags.Coverage))
	fmt.Fprintf(tw, "  verdicts on unflagged topics\t%d\n", flags.Unflagged)
	fmt.Fprintln(tw)

	next := r.NextActions
	fmt.Fprintln(tw, "next actions")
	fmt.Fprintf(tw, "  after a bundle\t%d\n", next.Actions)
	fmt.Fprintf(tw, "  asked\t%d\t%s\n", next.Asked.Count, percent(next.Asked.Share))
	fmt.Fprintf(tw, "  proceeded\t%d\t%s\n", next.Proceeded.Count, percent(next.Proceeded.Share))
	fmt.Fprintf(tw, "  asserted\t%d\t%s\n", next.Asserted.Count, percent(next.Asserted.Share))
	fmt.Fprintf(tw, "  with no bundle before them\t%d\n", next.Unattributed)
	return tw.Flush()
}

func percent(share *float64) string {
	if share == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *share*100)
}

func duration(s *float64) string {
	if s == nil {
		return "-"
	}
	return time.Duration(*s * float64(time.Second)).Round(time.Second).String()
}

func rate(r *float64) string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("%.3f", *r)
}

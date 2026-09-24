package eval

import (
	"encoding/json"
	"slices"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The sections of a bundle audit, in the order a report lists them: the keys
// of a bundle audit record's `sections`.
var bundleSections = []string{"scope.entities", "anchors", "stances", "recent", "open_questions", "conflicts"}

// The verdicts a consumer gives a flagged conflict.
const (
	verdictReal     = "real"
	verdictSpurious = "spurious"
)

// DrillDown is the drill-down rate per bundle section: for the session-linked
// bundles served in the window, how often the consumer followed an id a
// section served before the next bundle on that scope in that session.
type DrillDown struct {
	// Bundles is the session-linked bundles served in the window.
	Bundles int `json:"bundles"`
	// WithoutSession is the bundles served in the window with no session,
	// which nothing can follow and so are left out of every figure here.
	WithoutSession int `json:"bundles_without_session"`
	// Sections is one entry per bundle section, always all six, in a fixed
	// order.
	Sections []Section `json:"sections"`
	// LookedBeyond is the `search` and `resolve` calls that returned no id a
	// bundle they followed had served.
	LookedBeyond LookedBeyond `json:"looked_beyond"`
}

// Section is one bundle section's drill-down.
type Section struct {
	Section string `json:"section"`
	// Served is how many of the bundles served at least one id in this section.
	Served int `json:"served"`
	// Followed is how many of those the consumer followed one of those ids
	// from: a `get_l1`, `get_l0` or `stance_history` call on it, or a `search`
	// within it.
	Followed int `json:"followed"`
	// Share is Followed over Served; null where Served is zero.
	Share *float64 `json:"share"`
}

// LookedBeyond is the calls that went past what the bundle served, by call.
type LookedBeyond struct {
	Search  Beyond `json:"search"`
	Resolve Beyond `json:"resolve"`
}

// Beyond is one call's looking beyond the bundle.
type Beyond struct {
	// Calls is the successful calls made while a session-linked bundle in the
	// window was the latest on its scope in the session.
	Calls int `json:"calls"`
	// Beyond is how many of them returned no id any such bundle had served,
	// including those that returned nothing.
	Beyond int `json:"beyond"`
	// Share is Beyond over Calls; null where Calls is zero.
	Share *float64 `json:"share"`
}

// ConflictFlags is conflict-flag precision: of the conflicts session-linked
// bundles flagged, what the consumer said about them in its next actions.
type ConflictFlags struct {
	// Flagged is the conflicts the bundles flagged, one per bundle and topic.
	Flagged int `json:"flagged"`
	// Verdicts is how many of them the consumer judged, in a next action
	// attributed to the bundle that flagged them. A conflict judged twice
	// counts once, with the later verdict.
	Verdicts int `json:"verdicts"`
	Real     int `json:"real"`
	Spurious int `json:"spurious"`
	// Precision is Real over Real and Spurious; null where there is no
	// verdict.
	Precision *float64 `json:"precision"`
	// Coverage is Verdicts over Flagged; null where nothing was flagged.
	Coverage *float64 `json:"coverage"`
	// Unflagged is verdicts on a topic the bundle their next action is
	// attributed to did not flag. They are in none of the figures above.
	Unflagged int `json:"unflagged_verdicts"`
}

// NextActions is what the consumer did after a bundle: the next actions
// attributed to a session-linked bundle in the window.
type NextActions struct {
	// Actions is the next actions attributed to a bundle: each to the latest
	// bundle on its scope in its session before it, as `get_session` does.
	Actions   int    `json:"actions"`
	Asked     Action `json:"asked"`
	Proceeded Action `json:"proceeded"`
	Asserted  Action `json:"asserted"`
	// Unattributed is the next actions in the window with no bundle in it on
	// their scope in their session before them.
	Unattributed int `json:"unattributed"`
}

// Action is one kind of next action.
type Action struct {
	Count int `json:"count"`
	// Share is Count over Actions; null where there is none.
	Share *float64 `json:"share"`
}

// auditRecord is the part of a bundle or handle audit's payload.native this
// package reads (api.AuditRecord): ids and call names, and nothing that could
// carry content.
type auditRecord struct {
	Call          string              `json:"call"`
	Session       string              `json:"session"`
	SessionSource string              `json:"session_source"`
	Scope         string              `json:"scope"`
	Sections      map[string]auditIDs `json:"sections"`
	TargetIDs     []string            `json:"target_ids"`
	ReturnedIDs   []string            `json:"returned_ids"`
	Status        int                 `json:"status"`
}

type auditIDs struct {
	L1       []string `json:"l1"`
	Topics   []string `json:"topics"`
	Entities []string `json:"entities"`
}

func (a auditIDs) all() []string {
	return slices.Concat(a.L1, a.Topics, a.Entities)
}

// nextAction is the part of a `next_action` event's payload.native this
// package reads (agent.Native).
type nextAction struct {
	Session  string `json:"session"`
	Scope    string `json:"scope"`
	Action   string `json:"action"`
	Verdicts []struct {
		TopicID string `json:"topic_id"`
		Verdict string `json:"verdict"`
	} `json:"verdicts"`
}

// session names one agent session: the source it was posted to and its id.
type session struct{ source, id string }

// served is one session-linked bundle in the window.
type served struct {
	scope    string
	sections map[string]map[string]bool
	// every is every id the bundle served, in any section.
	every    map[string]bool
	followed map[string]bool
	flagged  map[string]bool
	verdicts map[string]string
}

// SummarizeSessions is the drill-down rate, conflict-flag precision and next
// actions over events: the `audit` events under Hearsay's own source and the
// `next_action` events of the window, in the order they happened
// ([l0.Store.Timeline]). A non-empty scope keeps the bundles and next actions
// on that bundle scope only. An audit or next action whose payload cannot be
// read is skipped: an operator deletion redacts it (ADR-0018).
func SummarizeSessions(events []connector.Event, scope string) (DrillDown, ConflictFlags, NextActions) {
	var (
		drill   DrillDown
		flags   ConflictFlags
		actions NextActions
		bundles []*served
		// latest is the latest bundle in the window on each scope of each
		// session: the one a handle call or next action follows.
		latest = map[session]map[string]*served{}
	)
	calls := map[string]*Beyond{"search": &drill.LookedBeyond.Search, "resolve": &drill.LookedBeyond.Resolve}
	counts := map[string]*Action{"asked": &actions.Asked, "proceeded": &actions.Proceeded, "asserted": &actions.Asserted}
	for _, ev := range events {
		switch {
		case ev.Kind == connector.KindAudit && ev.Source == connector.SelfSource:
			var rec auditRecord
			if json.Unmarshal(ev.Payload.Native, &rec) != nil {
				continue
			}
			s := session{rec.SessionSource, rec.Session}
			if rec.Call == "get_bundle" {
				if scope != "" && rec.Scope != scope {
					continue
				}
				if rec.Session == "" {
					drill.WithoutSession++
					continue
				}
				b := &served{scope: rec.Scope, sections: map[string]map[string]bool{}, every: map[string]bool{},
					followed: map[string]bool{}, flagged: map[string]bool{}, verdicts: map[string]string{}}
				for name, ids := range rec.Sections {
					if len(ids.all()) == 0 {
						continue
					}
					b.sections[name] = map[string]bool{}
					for _, id := range ids.all() {
						b.sections[name][id], b.every[id] = true, true
					}
				}
				for _, topic := range rec.Sections["conflicts"].Topics {
					b.flagged[topic] = true
				}
				bundles = append(bundles, b)
				if latest[s] == nil {
					latest[s] = map[string]*served{}
				}
				latest[s][rec.Scope] = b
				continue
			}
			// A handle call. One that was refused names no id.
			if rec.Session == "" || rec.Status != 0 || len(latest[s]) == 0 {
				continue
			}
			targets := slices.Clone(rec.TargetIDs)
			if rec.Call == "search" && rec.Scope != "" {
				// A search within an entity is a step into it.
				targets = append(targets, rec.Scope)
			}
			beyond := true
			for _, b := range latest[s] {
				for name, ids := range b.sections {
					if slices.ContainsFunc(targets, func(id string) bool { return ids[id] }) {
						b.followed[name] = true
					}
				}
				beyond = beyond && !slices.ContainsFunc(rec.ReturnedIDs, func(id string) bool { return b.every[id] })
			}
			if c := calls[rec.Call]; c != nil {
				c.Calls++
				if beyond {
					c.Beyond++
				}
			}
		case ev.Kind == connector.KindNextAction:
			var next nextAction
			if json.Unmarshal(ev.Payload.Native, &next) != nil || next.Session == "" {
				continue
			}
			if scope != "" && next.Scope != scope {
				continue
			}
			b := latest[session{ev.Source, next.Session}][next.Scope]
			c := counts[next.Action]
			if b == nil || c == nil {
				actions.Unattributed++
				continue
			}
			actions.Actions++
			c.Count++
			for _, v := range next.Verdicts {
				if !b.flagged[v.TopicID] {
					flags.Unflagged++
					continue
				}
				if v.Verdict == verdictReal || v.Verdict == verdictSpurious {
					b.verdicts[v.TopicID] = v.Verdict
				}
			}
		}
	}

	drill.Bundles = len(bundles)
	for _, name := range bundleSections {
		sec := Section{Section: name}
		for _, b := range bundles {
			if b.sections[name] != nil {
				sec.Served++
				if b.followed[name] {
					sec.Followed++
				}
			}
		}
		if sec.Served > 0 {
			sec.Share = ratio(sec.Followed, sec.Served)
		}
		drill.Sections = append(drill.Sections, sec)
	}
	for _, c := range calls {
		if c.Calls > 0 {
			c.Share = ratio(c.Beyond, c.Calls)
		}
	}

	for _, b := range bundles {
		flags.Flagged += len(b.flagged)
		for _, v := range b.verdicts {
			flags.Verdicts++
			if v == verdictReal {
				flags.Real++
			} else {
				flags.Spurious++
			}
		}
	}
	if flags.Verdicts > 0 {
		flags.Precision = ratio(flags.Real, flags.Verdicts)
	}
	if flags.Flagged > 0 {
		flags.Coverage = ratio(flags.Verdicts, flags.Flagged)
	}

	for _, c := range counts {
		if actions.Actions > 0 {
			c.Share = ratio(c.Count, actions.Actions)
		}
	}
	return drill, flags, actions
}

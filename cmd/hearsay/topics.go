package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

// runTopics is `hearsay topics`: a configured human reading a scope's topics
// and its topic ledger, and merging, splitting and undoing by hand
// (ADR-0020). Every read is the one the API serves that person
// (`stance_history`): the topics as the ledger makes them now, filtered by
// their reach and by what the evidence's access lists allow now. A topic, a
// stance or an operation they may not read is answered exactly as one that
// does not exist. The operations themselves are [l2.Operate], which refuses a
// person the scope's authority does not let ratify by hand.
func runTopics(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("topics", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	who := fs.String("principal", "", "configured human principal reading or changing topics")
	var stances repeated
	fs.Var(&stances, "stance", "for split: a stance to move to the new topic; repeat the flag for several")
	name := fs.String("name", "", "for split: the new topic's name")
	scope := fs.String("scope", "", "for ops: only operations in this scope")
	since := fs.String("since", "", "for ops: only operations recorded at or after this RFC3339 time")
	asJSON := fs.Bool("json", false, "for ops: print the operations as JSON")
	action, words, err := parseAction(fs, args, "")
	if err != nil {
		return err
	}
	// Everything about the command line is settled before anything is read,
	// so a typo is a typo rather than a connection failure.
	var undoes int64
	var from time.Time
	switch action {
	case "list":
		if err := checkFlags(fs, action, "config", "principal"); err != nil {
			return err
		}
		if len(words) != 1 {
			return errors.New("list takes one scope")
		}
	case "merge":
		if err := checkFlags(fs, action, "config", "principal"); err != nil {
			return err
		}
		if len(words) != 2 {
			return errors.New("merge takes the topic to merge and the topic it goes into")
		}
	case "split":
		if err := checkFlags(fs, action, "config", "principal", "stance", "name"); err != nil {
			return err
		}
		switch {
		case len(words) != 1:
			return errors.New("split takes one topic")
		case len(stances) == 0:
			return errors.New("split needs --stance, once for each stance to move")
		case strings.TrimSpace(*name) == "":
			return errors.New("split needs --name for the new topic")
		}
	case "undo":
		if err := checkFlags(fs, action, "config", "principal"); err != nil {
			return err
		}
		if len(words) != 1 {
			return errors.New("undo takes one operation id")
		}
		if undoes, err = strconv.ParseInt(words[0], 10, 64); err != nil || undoes <= 0 {
			return fmt.Errorf("%q is not an operation id", words[0])
		}
	case "ops":
		if err := checkArgs(fs, action, words, "config", "principal", "scope", "since", "json"); err != nil {
			return err
		}
		if *since != "" {
			if from, err = time.Parse(time.RFC3339, *since); err != nil {
				return fmt.Errorf("--since %q is not an RFC3339 time", *since)
			}
		}
	case "":
		return errors.New("no action given: want list, merge, split, undo or ops")
	default:
		return fmt.Errorf("unknown action %q: want list, merge, split, undo or ops", action)
	}
	if *configPath == "" || *who == "" {
		return errors.New("topics needs --config and --principal <human-id>")
	}
	repo, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	human, ok := repo.Principal(*who)
	if !ok {
		return fmt.Errorf("unknown principal %q: topics are read and changed by a configured human", *who)
	}
	if human.Kind != principal.KindHuman {
		return fmt.Errorf("principal %q is of kind %q: topics are read and changed by a configured human", *who, human.Kind)
	}
	resolveDatabase()
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	v, err := newTopicView(ctx, pool, repo, human)
	if err != nil {
		return err
	}
	switch action {
	case "list":
		return v.list(ctx, stdout, words[0])
	case "ops":
		return v.ops(ctx, stdout, l2.OperationFilter{Scope: *scope, Since: from}, *asJSON)
	}
	req := l2.OperationRequest{Principal: human.ID}
	switch action {
	case "merge":
		req.Kind, req.From, req.Into = l2.OperationMerge, words[0], words[1]
		err = v.checkMerge(ctx, req)
	case "split":
		req.Kind, req.Topic, req.Name, req.Stances = l2.OperationSplit, words[0], *name, stances
		err = v.checkSplit(ctx, req)
	case "undo":
		req.Kind, req.Undoes = l2.OperationUndo, undoes
		err = v.checkUndo(ctx, undoes)
	}
	if err != nil {
		return err
	}
	op, err := l2.Operate(ctx, pool, repo, req)
	if err != nil {
		return v.conflict(ctx, err)
	}
	fmt.Fprintln(stdout, op.ID)
	return nil
}

// topicView is the graph as one person reads it: the reader the API builds
// for a person calling it directly ([l2.View]).
type topicView struct {
	*l2.View
}

func newTopicView(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, human principal.Principal) (*topicView, error) {
	v, err := l2.NewView(ctx, l2.New(pool), repo, human)
	if err != nil {
		return nil, err
	}
	return &topicView{v}, nil
}

// operation reports whether the person may read an operation: every topic it
// covers reads, as the ledger makes it now, as a topic they may read.
func (v *topicView) operation(ctx context.Context, op l2.Operation) (bool, error) {
	for _, id := range op.Topics {
		a, err := v.Topic(ctx, id)
		if err != nil || a == nil {
			return false, err
		}
	}
	return true, nil
}

func noTopic(id string) error { return fmt.Errorf("no topic %q", id) }

func noOperation(id int64) error { return fmt.Errorf("no operation %d", id) }

func (v *topicView) checkMerge(ctx context.Context, req l2.OperationRequest) error {
	for _, id := range []string{req.From, req.Into} {
		if a, err := v.Topic(ctx, id); err != nil || a == nil {
			return errors.Join(err, noTopic(id))
		}
	}
	return nil
}

// checkSplit refuses a split of a topic the person may not read, or naming a
// stance they may not read on it: a split that moved a stance they cannot see
// would tell them where it went.
func (v *topicView) checkSplit(ctx context.Context, req l2.OperationRequest) error {
	a, err := v.Topic(ctx, req.Topic)
	if err != nil || a == nil {
		return errors.Join(err, noTopic(req.Topic))
	}
	for _, id := range req.Stances {
		i := slices.IndexFunc(a.History, func(st l2.Stance) bool { return st.ID == id })
		if i < 0 || !v.Visible(a, a.History[i]) {
			return fmt.Errorf("no stance %q on topic %q", id, req.Topic)
		}
	}
	return nil
}

func (v *topicView) checkUndo(ctx context.Context, id int64) error {
	op, err := v.Store().Operation(ctx, id)
	if errors.Is(err, l2.ErrNotFound) {
		return noOperation(id)
	}
	if err != nil {
		return err
	}
	if ok, err := v.operation(ctx, op); err != nil || !ok {
		return errors.Join(err, noOperation(id))
	}
	return nil
}

// conflict is an operation's refusal as the person may read it: an undo in
// conflict names only the operations in the way that they may read.
func (v *topicView) conflict(ctx context.Context, err error) error {
	var c *l2.ConflictError
	if !errors.As(err, &c) {
		return err
	}
	shown := &l2.ConflictError{Operation: c.Operation, Reason: c.Reason}
	for _, id := range c.Conflicting {
		op, err := v.Store().Operation(ctx, id)
		if err != nil {
			continue
		}
		if ok, err := v.operation(ctx, op); err == nil && ok {
			shown.Conflicting = append(shown.Conflicting, id)
		}
	}
	if len(shown.Conflicting) == 0 {
		return fmt.Errorf("%w: operation %d cannot be undone: later operations on its topics are still in force", l2.ErrConflict, c.Operation)
	}
	return shown
}

// list prints the topics in a scope the person may read, oldest first, with
// the number of stances in each one's history they may see. A scope that
// does not exist and a scope with nothing they may read both print the
// header alone.
func (v *topicView) list(ctx context.Context, w io.Writer, scope string) error {
	assessed, err := v.Topics(ctx, scope)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTANCES\tNAME")
	for i := range assessed {
		a := &assessed[i]
		n := 0
		for _, st := range a.History {
			if v.Visible(a, st) {
				n++
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", a.Topic.ID, n, a.Topic.Name)
	}
	return tw.Flush()
}

// operationRecord is one operation as `topics ops --json` prints it.
type operationRecord struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	Principal string    `json:"principal"`
	At        time.Time `json:"at"`
	// Topics are every topic the operation covers; Stances only those the
	// person may read.
	Topics   []string `json:"topics"`
	Stances  []string `json:"stances"`
	Name     string   `json:"name,omitempty"`
	Undoes   int64    `json:"undoes,omitempty"`
	Undone   bool     `json:"undone"`
	UndoneBy int64    `json:"undone_by,omitempty"`
}

// ops prints the ledger the person may read, oldest first.
func (v *topicView) ops(ctx context.Context, w io.Writer, f l2.OperationFilter, asJSON bool) error {
	ops, err := v.Store().Operations(ctx, f)
	if err != nil {
		return err
	}
	records := []operationRecord{}
	stances := map[string]bool{}
	for _, op := range ops {
		ok, err := v.operation(ctx, op)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		rec := operationRecord{
			ID: op.ID, Kind: string(op.Kind), Scope: op.Scope, Principal: op.Principal, At: op.At,
			Topics: op.Topics, Stances: []string{}, Name: op.Name, Undoes: op.Undoes,
			Undone: op.UndoneBy != 0, UndoneBy: op.UndoneBy,
		}
		for _, id := range op.Stances {
			seen, err := v.stance(ctx, stances, id)
			if err != nil {
				return err
			}
			if seen {
				rec.Stances = append(rec.Stances, id)
			}
		}
		records = append(records, rec)
	}
	if asJSON {
		return json.NewEncoder(w).Encode(records)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tKIND\tSCOPE\tPRINCIPAL\tUNDOES\tUNDONE_BY\tTOPICS\tSTANCES\tNAME")
	for _, r := range records {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.At.Format(time.RFC3339), r.Kind, r.Scope, r.Principal,
			orDash(r.Undoes), orDash(r.UndoneBy), joinOrDash(r.Topics), joinOrDash(r.Stances), r.Name)
	}
	return tw.Flush()
}

// stance reports whether the person may see a stance an operation covers,
// wherever the ledger puts it now, remembering the answer in seen.
func (v *topicView) stance(ctx context.Context, seen map[string]bool, id string) (bool, error) {
	if ok, done := seen[id]; done {
		return ok, nil
	}
	seen[id] = false
	st, err := v.Store().Stance(ctx, id)
	if errors.Is(err, l2.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a, err := v.Topic(ctx, st.TopicID)
	if err != nil || a == nil {
		return false, err
	}
	i := slices.IndexFunc(a.History, func(h l2.Stance) bool { return h.ID == id })
	seen[id] = i >= 0 && v.Visible(a, a.History[i])
	return seen[id], nil
}

func orDash(id int64) string {
	if id == 0 {
		return "-"
	}
	return strconv.FormatInt(id, 10)
}

func joinOrDash(ids []string) string {
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}

// repeated collects a flag given once per value.
type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }

func (r *repeated) Set(v string) error {
	if v == "" {
		return errors.New("empty value")
	}
	*r = append(*r, v)
	return nil
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

// runGestures records human gestures through the shared L2 ledger. Visibility
// is checked with the same direct-person reader used by topics and the API.
func runGestures(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("gestures", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	who := fs.String("principal", "", "configured human principal")
	scope := fs.String("scope", "", "for list: only this scope")
	since := fs.String("since", "", "for list: RFC3339 lower time bound")
	asJSON := fs.Bool("json", false, "for list: print JSON")
	var artifactSource, artifactID string
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--artifact" || args[i] == "-artifact" {
			if artifactSource != "" {
				return errors.New("--artifact given more than once")
			}
			if i+2 >= len(args) || strings.HasPrefix(args[i+1], "-") || strings.HasPrefix(args[i+2], "-") {
				return errors.New("--artifact needs <source> <artifact-id>")
			}
			artifactSource, artifactID = args[i+1], args[i+2]
			i += 2
			continue
		}
		filtered = append(filtered, args[i])
	}
	action, words, err := parseAction(fs, filtered, "")
	if err != nil {
		return err
	}
	var from time.Time
	var undoID int64
	switch action {
	case "ratify", "demote", "pin", "unpin":
		if err := checkFlags(fs, action, "config", "principal"); err != nil {
			return err
		}
		if (len(words) != 1 || artifactSource != "") && (len(words) != 0 || artifactSource == "") {
			return fmt.Errorf("%s needs one document id or --artifact <source> <artifact-id>", action)
		}
	case "undo":
		if err := checkFlags(fs, action, "config", "principal"); err != nil {
			return err
		}
		if artifactSource != "" || len(words) != 1 {
			return errors.New("undo takes one gesture id")
		}
		undoID, err = strconv.ParseInt(words[0], 10, 64)
		if err != nil || undoID <= 0 {
			return fmt.Errorf("%q is not a gesture id", words[0])
		}
	case "list":
		if artifactSource != "" {
			return errors.New("list does not read --artifact")
		}
		if err := checkArgs(fs, action, words, "config", "principal", "scope", "since", "json"); err != nil {
			return err
		}
		if *since != "" {
			from, err = time.Parse(time.RFC3339, *since)
			if err != nil {
				return fmt.Errorf("--since %q is not an RFC3339 time", *since)
			}
		}
	case "":
		return errors.New("no action given: want ratify, demote, pin, unpin, undo or list")
	default:
		return fmt.Errorf("unknown action %q: want ratify, demote, pin, unpin, undo or list", action)
	}
	if *configPath == "" || *who == "" {
		return errors.New("gestures needs --config and --principal <human-id>")
	}
	repo, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	human, ok := repo.Principal(*who)
	if !ok {
		return fmt.Errorf("unknown principal %q: gestures require a configured human", *who)
	}
	if human.Kind != principal.KindHuman {
		return fmt.Errorf("principal %q is of kind %q: gestures require a configured human", *who, human.Kind)
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
	gv := gestureView{pool: pool, store: v.store, docs: l1.New(pool), reader: v.reader, topics: v}
	if action == "list" {
		return gv.list(ctx, stdout, *scope, from, *asJSON)
	}
	var target l2.Gesture
	if action == "undo" {
		target, err = gv.gesture(ctx, undoID)
		if err != nil {
			return err
		}
	} else {
		ids, err := gv.documents(ctx, words, artifactSource, artifactID)
		if err != nil {
			return err
		}
		if action == "unpin" {
			pins, err := gv.activePins(ctx, ids)
			if err != nil {
				return err
			}
			for _, pin := range pins {
				if err := gv.write(ctx, stdout, repo, human.ID, l2.GestureUndo, nil, pin.Event); err != nil {
					return err
				}
			}
			return nil
		}
		return gv.write(ctx, stdout, repo, human.ID, l2.GestureAction(action), ids, "")
	}
	return gv.write(ctx, stdout, repo, human.ID, l2.GestureUndo, nil, target.Event)
}

type gestureView struct {
	pool   *pgxpool.Pool
	store  *l2.Store
	docs   *l1.Store
	reader l1.Reader
	topics *topicView
}

func noDocument(id string) error { return fmt.Errorf("no document %q", id) }
func noGesture(id int64) error   { return fmt.Errorf("no gesture %d", id) }

// An artifact can make several L1 documents (for example, meeting segments).
// Match its L0 artifact provenance, then apply the same visibility check to
// every document it resolves to. No match and a hidden match have one result.
func (v gestureView) documents(ctx context.Context, words []string, source, artifact string) ([]string, error) {
	var ids []string
	label := ""
	if source == "" {
		label = words[0]
		if _, _, err := l1.ParseDocID(label); err != nil {
			return nil, noDocument(label)
		}
		ids = []string{label}
	} else {
		label = source + " " + artifact
		if !connector.ValidSourceID(source) {
			return nil, noDocument(label)
		}
		rows, err := v.pool.Query(ctx, `SELECT DISTINCT d.id FROM l1_docs d JOIN l0_events e ON e.id = ANY(d.l0_refs) WHERE e.source=$1 AND e.artifact=$2 ORDER BY d.id`, source, artifact)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return nil, noDocument(label)
	}
	for _, id := range ids {
		doc, err := v.docs.Get(ctx, id)
		if errors.Is(err, l1.ErrNotFound) {
			return nil, noDocument(label)
		}
		if err != nil {
			return nil, err
		}
		if !v.reader.MayRead(doc.Document) {
			return nil, noDocument(label)
		}
	}
	return ids, nil
}

func (v gestureView) readable(ctx context.Context, g l2.Gesture) (bool, []string, error) {
	for _, id := range g.Documents {
		doc, err := v.docs.Get(ctx, id)
		if errors.Is(err, l1.ErrNotFound) {
			return false, nil, nil
		}
		if err != nil {
			return false, nil, err
		}
		if !v.reader.MayRead(doc.Document) {
			return false, nil, nil
		}
	}
	visible := []string{}
	for _, id := range g.Stances {
		ok, err := v.topics.stance(ctx, map[string]bool{}, id)
		if err != nil {
			return false, nil, err
		}
		if ok {
			visible = append(visible, id)
		}
	}
	return true, visible, nil
}

func (v gestureView) gesture(ctx context.Context, id int64) (l2.Gesture, error) {
	all, err := v.store.Gestures(ctx, "")
	if err != nil {
		return l2.Gesture{}, err
	}
	for _, g := range all {
		if g.ID == id {
			ok, _, err := v.readable(ctx, g)
			if err != nil {
				return l2.Gesture{}, err
			}
			if ok {
				return g, nil
			}
			break
		}
	}
	return l2.Gesture{}, noGesture(id)
}

func (v gestureView) activePins(ctx context.Context, docs []string) ([]l2.Gesture, error) {
	all, err := v.store.Gestures(ctx, "")
	if err != nil {
		return nil, err
	}
	var pins []l2.Gesture
	for i := len(all) - 1; i >= 0; i-- {
		g := all[i]
		if g.Action == l2.GesturePin && g.UndoneBy == 0 && strings.Join(g.Documents, "\x00") == strings.Join(docs, "\x00") {
			ok, _, err := v.readable(ctx, g)
			if err != nil {
				return nil, err
			}
			if ok {
				pins = append(pins, g)
			}
		}
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("no active pin for document %q", strings.Join(docs, ","))
	}
	return pins, nil
}

func (v gestureView) write(ctx context.Context, w io.Writer, repo config.Repo, who string, action l2.GestureAction, docs []string, undoes string) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	req := l2.GestureRequest{Event: connector.EventID("hearsay", "cli-gesture-"+hex.EncodeToString(random[:])), Principal: who, Action: action, Documents: docs, Undoes: undoes}
	g, _, err := l2.RecordGesture(ctx, v.pool, repo, req)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, g.ID)
	return err
}

type gestureRecord struct {
	ID        int64            `json:"id"`
	Action    l2.GestureAction `json:"action"`
	Scope     string           `json:"scope"`
	Principal string           `json:"principal"`
	At        time.Time        `json:"at"`
	Documents []string         `json:"documents"`
	Stances   []string         `json:"stances"`
	Pins      []l2.Pin         `json:"pins"`
	Undoes    int64            `json:"undoes,omitempty"`
	UndoneBy  int64            `json:"undone_by,omitempty"`
}

func (v gestureView) list(ctx context.Context, w io.Writer, scope string, since time.Time, asJSON bool) error {
	all, err := v.store.Gestures(ctx, scope)
	if err != nil {
		return err
	}
	records := []gestureRecord{}
	for _, g := range all {
		if !since.IsZero() && g.At.Before(since) {
			continue
		}
		ok, stances, err := v.readable(ctx, g)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		records = append(records, gestureRecord{ID: g.ID, Action: g.Action, Scope: g.Scope, Principal: g.Principal, At: g.At, Documents: g.Documents, Stances: stances, Pins: g.Pins, Undoes: g.Undoes, UndoneBy: g.UndoneBy})
	}
	if asJSON {
		return json.NewEncoder(w).Encode(records)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tACTION\tSCOPE\tPRINCIPAL\tUNDOES\tUNDONE_BY\tDOCUMENTS\tSTANCES\tPINS")
	for _, r := range records {
		pins := make([]string, len(r.Pins))
		for i, p := range r.Pins {
			pins[i] = p.Scope + ":" + p.L1
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.At.Format(time.RFC3339), r.Action, r.Scope, r.Principal, orDash(r.Undoes), orDash(r.UndoneBy), joinOrDash(r.Documents), joinOrDash(r.Stances), joinOrDash(pins))
	}
	return tw.Flush()
}

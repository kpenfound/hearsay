// Package deletion expands operator selectors, walks their forward provenance,
// applies a deletion, and reads the record of each one and what it rebuilt.
package deletion

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Selector names exactly one set of L0 events. Author is either a configured
// principal or source:native-id (source:@handle for a handle-only identity).
type Selector struct {
	Event, ArtifactSource, ArtifactID, Author string
}

// Preview is the complete forward walk. All slices are sorted and non-nil.
type Preview struct {
	Selector        Selector `json:"selector"`
	Reason          string   `json:"reason"`
	Events          []string `json:"events"`
	Documents       []string `json:"documents"`
	Stances         []string `json:"stances"`
	Topics          []string `json:"topics"`
	AliasCandidates []string `json:"alias_candidates"`
	Pins            []string `json:"pins"`
	// Gestures are the gestures in force the events made, in ledger order:
	// deleting their events takes them out of force.
	Gestures []int64 `json:"gestures"`
}

// Counts returns the number of affected objects in each layer.
func (p Preview) Counts() map[string]int {
	return map[string]int{
		"events": len(p.Events), "documents": len(p.Documents),
		"stances": len(p.Stances), "topics": len(p.Topics),
		"alias_candidates": len(p.AliasCandidates), "pins": len(p.Pins),
		"gestures": len(p.Gestures),
	}
}

// Walk uses one read-only snapshot so every layer describes the same database
// state. [Apply] runs the same walk inside its write transaction.
func Walk(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, sel Selector, reason string) (Preview, error) {
	p := emptyPreview(sel, reason)
	if err := check(sel, reason); err != nil {
		return p, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return p, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return walk(ctx, tx, repo, sel, reason)
}

func emptyPreview(sel Selector, reason string) Preview {
	return Preview{Selector: sel, Reason: reason, Events: []string{}, Documents: []string{}, Stances: []string{}, Topics: []string{}, AliasCandidates: []string{}, Pins: []string{}, Gestures: []int64{}}
}

// check refuses a selector or reason before any database is touched.
func check(sel Selector, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	n := 0
	if sel.Event != "" {
		n++
	}
	if sel.ArtifactSource != "" || sel.ArtifactID != "" {
		n++
		if !connector.ValidSourceID(sel.ArtifactSource) || sel.ArtifactID == "" {
			return fmt.Errorf("--artifact needs a valid source and artifact id")
		}
	}
	if sel.Author != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("give exactly one of --event, --artifact, or --author")
	}
	if sel.Event != "" {
		if _, _, err := connector.ParseEventID(sel.Event); err != nil {
			return err
		}
	}
	return nil
}

func walk(ctx context.Context, tx pgx.Tx, repo config.Repo, sel Selector, reason string) (Preview, error) {
	p := emptyPreview(sel, reason)
	var (
		rows pgx.Rows
		err  error
	)
	switch {
	case sel.Event != "":
		rows, err = tx.Query(ctx, `SELECT id FROM l0_events WHERE id=$1 ORDER BY id`, sel.Event)
	case sel.ArtifactSource != "":
		rows, err = tx.Query(ctx, `SELECT id FROM l0_events WHERE source=$1 AND artifact=$2 ORDER BY id`, sel.ArtifactSource, sel.ArtifactID)
	default:
		identities, e := expandAuthor(repo, sel.Author)
		if e != nil {
			return p, e
		}
		for _, id := range identities {
			var found pgx.Rows
			if id.NativeID != "" {
				found, err = tx.Query(ctx, `SELECT id FROM l0_events WHERE payload->'author'->>'source'=$1 AND payload->'author'->>'native_id'=$2 ORDER BY id`, id.Source, id.NativeID)
			} else {
				found, err = tx.Query(ctx, `SELECT id FROM l0_events WHERE payload->'author'->>'source'=$1 AND lower(payload->'author'->>'handle')=$2 ORDER BY id`, id.Source, principal.FoldHandle(id.Handle))
			}
			if err != nil {
				return p, err
			}
			ids, e := readIDs(found)
			if e != nil {
				return p, e
			}
			p.Events = append(p.Events, ids...)
		}
	}
	if rows != nil {
		if err != nil {
			return p, err
		}
		p.Events, err = readIDs(rows)
		if err != nil {
			return p, err
		}
	}
	p.Events = unique(p.Events)
	if len(p.Events) == 0 {
		return p, fmt.Errorf("selector matches no L0 events")
	}
	gestures, err := tx.Query(ctx, `SELECT g.id FROM l2_gestures g
WHERE g.event = ANY($1) AND g.action <> 'undo' AND NOT EXISTS (SELECT 1 FROM l2_gestures u WHERE u.undoes = g.id)
  AND NOT EXISTS (SELECT 1 FROM l0_events e WHERE e.id = g.event AND e.deletion IS NOT NULL)
ORDER BY g.id`, p.Events)
	if err != nil {
		return p, err
	}
	if p.Gestures, err = pgx.CollectRows(gestures, pgx.RowTo[int64]); err != nil {
		return p, err
	}
	queries := []struct {
		dest *[]string
		sql  string
		arg  any
	}{
		{&p.Documents, `SELECT id FROM l1_docs WHERE l0_refs && $1 ORDER BY id`, p.Events},
	}
	for _, q := range queries {
		if *q.dest, err = queryIDs(ctx, tx, q.sql, q.arg); err != nil {
			return p, err
		}
	}
	if len(p.Documents) == 0 {
		return p, nil
	}
	if p.Stances, err = queryIDs(ctx, tx, `SELECT id FROM l2_stances WHERE evidence && $1 ORDER BY id`, p.Documents); err != nil {
		return p, err
	}
	if p.Topics, err = queryIDs(ctx, tx, `SELECT id FROM l2_topics WHERE opened_by = ANY($1) ORDER BY id`, p.Documents); err != nil {
		return p, err
	}
	if p.AliasCandidates, err = queryIDs(ctx, tx, `SELECT c.entity_id || ':' || c.alias FROM l2_alias_candidates c
WHERE c.evidence && $1 OR EXISTS (
  SELECT 1 FROM l2_alias_votes v WHERE v.entity_id=c.entity_id AND v.alias=c.alias
  AND (v.doc_id = ANY($1) OR v.pr_doc_id = ANY($1)))
ORDER BY c.entity_id, c.alias`, p.Documents); err != nil {
		return p, err
	}
	if p.Pins, err = queryIDs(ctx, tx, `SELECT scope || ':' || l1 FROM l2_pins WHERE l1 = ANY($1) ORDER BY scope, l1`, p.Documents); err != nil {
		return p, err
	}
	return p, nil
}

func expandAuthor(repo config.Repo, author string) ([]principal.Identity, error) {
	if p, ok := repo.Principal(author); ok {
		if len(p.Identities) == 0 {
			return nil, fmt.Errorf("principal %q has no source identities", author)
		}
		return p.Identities, nil
	}
	source, id, ok := strings.Cut(author, ":")
	if !ok || !connector.ValidSourceID(source) || id == "" {
		return nil, fmt.Errorf("unknown author %q: use a configured principal or source:native-id", author)
	}
	if len(repo.Sources) > 0 {
		if _, ok := repo.Source(source); !ok {
			return nil, fmt.Errorf("unknown source identity %q: source is not configured", author)
		}
	}
	if strings.HasPrefix(id, "@") {
		if len(id) == 1 {
			return nil, fmt.Errorf("empty author handle")
		}
		return []principal.Identity{{Source: source, Handle: id[1:]}}, nil
	}
	return []principal.Identity{{Source: source, NativeID: id}}, nil
}

func queryIDs(ctx context.Context, tx pgx.Tx, sql string, arg any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, arg)
	if err != nil {
		return nil, err
	}
	return readIDs(rows)
}

func readIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func unique(ids []string) []string {
	slices.Sort(ids)
	return slices.Compact(ids)
}

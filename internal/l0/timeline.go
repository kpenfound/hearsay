package l0

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Timeline returns every visible event of one of kinds that happened at or
// after since and before until, in the order they happened: by the revision's
// own edit time where it has one, else by the event's time, then by arrival.
// That is the order `get_session` reads a session in — an agent session's
// events all carry the session's start as their time, and when each happened
// is its revision's edit time. A zero since is the beginning; until is
// required.
//
// Unlike [Store.List] it is not capped: `hearsay eval` reads a whole window,
// and a metric computed over its first page would be wrong without saying so.
func (s *Store) Timeline(ctx context.Context, kinds []connector.Kind, since, until time.Time) ([]connector.Event, error) {
	if len(kinds) == 0 {
		return nil, errors.New("a timeline needs at least one kind")
	}
	if until.IsZero() {
		return nil, errors.New("a timeline needs an end")
	}
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	rows, err := s.db.Query(ctx, `
SELECT `+eventColumns+`
  FROM l0_events e
 WHERE `+visibleSQL+`
   AND e.kind = ANY($1)
   AND coalesce(e.revision_edited_at, e.occurred_at) >= $2
   AND coalesce(e.revision_edited_at, e.occurred_at) < $3
 ORDER BY coalesce(e.revision_edited_at, e.occurred_at), e.seq`, names, since, until)
	if err != nil {
		return nil, fmt.Errorf("reading the timeline: %w", err)
	}
	defer rows.Close()
	events := []connector.Event{}
	for rows.Next() {
		var (
			ev      connector.Event
			kind    string
			payload []byte
			acl     []byte
		)
		if err := rows.Scan(&ev.ID, &ev.Source, &ev.NativeID, &kind, &ev.Time, &payload, &acl); err != nil {
			return nil, fmt.Errorf("reading the timeline: %w", err)
		}
		if err := decodeInto(&ev, kind, payload, acl); err != nil {
			return nil, fmt.Errorf("reading the timeline: %w", err)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the timeline: %w", err)
	}
	return events, nil
}

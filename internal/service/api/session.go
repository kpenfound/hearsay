package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// sessionSource resolves a session only when its start event belongs to this
// caller. Its ACL and identities come from the connector, not the header.
func (c *Calls) sessionSource(ctx context.Context, caller Caller, reader l1.Reader, session, specified string) (string, error) {
	var found string
	for _, source := range c.sessionSources {
		if specified != "" && source != specified {
			continue
		}
		ev, err := c.events.Get(ctx, connector.EventID(source, session+"@start"))
		if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
			continue
		}
		if err != nil {
			return "", err
		}
		if ev.Kind != connector.KindAgentSession || ev.Payload.Artifact != session || !reader.Allows(ev.ACL) ||
			ev.Payload.Author == nil || ev.Payload.Author.Kind != connector.IdentityAgent || ev.Payload.Author.Source != source ||
			(caller.Agent != "" && ev.Payload.Author.NativeID != caller.Agent) ||
			len(ev.Payload.Participants) != 1 || ev.Payload.Participants[0].Identity.Source != source ||
			ev.Payload.Participants[0].Identity.Kind != connector.IdentityUser ||
			ev.Payload.Participants[0].Identity.NativeID != caller.Principal {
			continue
		}
		current, err := c.events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: source, Artifact: session}, Limit: 1})
		if err != nil {
			return "", err
		}
		if len(current) != 1 || !reader.Allows(current[0].ACL) {
			continue
		}
		if found != "" {
			return "", fail(http.StatusConflict, "session id is ambiguous across agent sources")
		}
		found = source
	}
	if found == "" {
		return "", fail(http.StatusNotFound, "no session %q", session)
	}
	return found, nil
}

// SessionTrace is a session's connector events and the bundle audits made in
// it, in occurrence order. The assertion id is the handle used to find it.
type SessionTrace struct {
	Source                  string            `json:"source"`
	Artifact                string            `json:"artifact"`
	Events                  []connector.Event `json:"events"`
	BundleAuditByNextAction map[string]string `json:"bundle_audit_by_next_action"`
}

func getSession(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		Assertion string `json:"assertion"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("assertion", args.Assertion); err != nil {
		return nil, err
	}
	notFound := fail(http.StatusNotFound, "no session for assertion %q", args.Assertion)
	ev, err := c.events.Get(ctx, args.Assertion)
	if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if ev.Source != AuditSource || ev.Kind != connector.KindAssertion {
		return nil, notFound
	}
	as, err := l2.AssertionOf(ev)
	if err != nil || as.Session == "" || as.Principal != caller.Principal ||
		(caller.Agent != "" && as.Agent != caller.Agent) {
		return nil, notFound
	}
	topic, err := c.graph.Topic(ctx, as.Topic)
	if errors.Is(err, l2.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	stance := l2.Stance{Evidence: as.Evidence}
	access, err := c.graph.Access(ctx, []l2.Topic{topic}, []l2.Stance{stance})
	if err != nil {
		return nil, err
	}
	assessed, err := c.graph.Assess(ctx, c.authority, reader, []l2.Topic{topic})
	if err != nil {
		return nil, err
	}
	if !assessed[0].Access.Topic(reader, topic) || !access.Stance(reader, stance) {
		return nil, notFound
	}
	source, err := c.sessionSource(ctx, caller, reader, as.Session, as.SessionSource)
	if err != nil {
		var callErr *Error
		if errors.As(err, &callErr) {
			return nil, notFound
		}
		return nil, err
	}
	rows, err := c.db.Query(ctx, `
SELECT id FROM l0_events
WHERE (source = $1 AND payload->>'artifact' = $2)
   OR (source = $3 AND kind = $4 AND payload->'native'->>'session' = $2
       AND payload->'native'->>'session_source' = $1)
ORDER BY coalesce(revision_edited_at, occurred_at), seq`, source, as.Session, AuditSource, string(connector.KindAudit))
	if err != nil {
		return nil, fmt.Errorf("listing session trace: %w", err)
	}
	defer rows.Close()
	trace := SessionTrace{Source: source, Artifact: as.Session, Events: []connector.Event{}, BundleAuditByNextAction: map[string]string{}}
	latestBundle := map[string]string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading session trace: %w", err)
		}
		item, err := c.events.Get(ctx, id)
		if errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading session event: %w", err)
		}
		if item.Kind == connector.KindAudit {
			var audit AuditRecord
			if err := json.Unmarshal(item.Payload.Native, &audit); err != nil {
				return nil, fmt.Errorf("decoding session audit: %w", err)
			}
			// A historical audit may concern a scope the caller can no longer
			// reach. Fail closed on grants even though the session still allows
			// the caller.
			if audit.Principal == caller.Principal && audit.Agent == as.Agent && reader.Effective.Grant.Scopes.Has(audit.Scope) {
				trace.Events = append(trace.Events, item)
				if audit.Call == "get_bundle" {
					latestBundle[audit.Scope] = item.ID
				}
			}
		} else if reader.Allows(item.ACL) {
			// Session revisions may carry their own tightened ACLs.
			if item.Kind == connector.KindNextAction {
				var next struct {
					Scope string `json:"scope"`
				}
				if err := json.Unmarshal(item.Payload.Native, &next); err != nil {
					return nil, fmt.Errorf("decoding session next action: %w", err)
				}
				if !reader.Effective.Grant.Scopes.Has(next.Scope) {
					continue
				}
				if bundleID := latestBundle[next.Scope]; bundleID != "" {
					trace.BundleAuditByNextAction[item.ID] = bundleID
				}
			}
			trace.Events = append(trace.Events, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing session trace: %w", err)
	}
	return trace, nil
}

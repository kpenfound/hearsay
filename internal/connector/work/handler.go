package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Handler authenticates and validates one artifact, then emits it and completes
// project access propagation before the shared push handler can answer 202.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	reader, _ := sink.(connector.ArtifactReader)
	return push.Handler(push.Options[Request]{
		Authenticate: func(_ context.Context, token string) (string, bool) {
			if id, ok := c.tokens.Match(token); ok {
				return id, true
			}
			return c.excluded.Match(token)
		},
		Process: func(ctx context.Context, poster string, req Request) (push.Result, error) {
			if !c.posters[poster] {
				return push.Result{}, push.Refuse(push.ErrForbidden, "poster is not allowed by posters")
			}
			artifact, err := c.validate(req)
			if err != nil {
				return push.Result{}, err
			}
			// Attribution is authorization, including on a deletion (whose shape
			// already refuses author). Nothing untrusted reaches an identity hint.
			if req.fields["author"] {
				p, ok := c.principals[req.Author]
				if !c.attributors[poster] {
					return push.Result{}, push.Refuse(push.ErrForbidden, "poster is not allowed to attribute author")
				}
				if !ok || (p.Kind != principal.KindHuman && p.Kind != principal.KindAgent) {
					return push.Result{}, push.Refuse(push.ErrForbidden, "author is not a configured human or agent principal")
				}
			}
			if reader == nil {
				return push.Result{}, errors.New("work requires an artifact reader")
			}
			unlock := c.lock(req.Project)
			defer unlock()
			var ev connector.Event
			held := true
			if req.Deleted {
				ev, held, err = push.Tombstone(ctx, reader, c.source, artifact)
			} else {
				ev, err = c.event(ctx, reader, poster, artifact, req)
			}
			if err == nil && held {
				err = c.emit(ctx, sink, reader, ev)
			}
			// Always use the project's *current* revision, even after an identical
			// repost or a stale revision that L0 stores without making it current.
			if err == nil && req.Kind == "project" && !req.Deleted {
				err = c.reconcile(ctx, sink, reader, req.Project)
			}
			return push.Result{Event: ev, NoContent: !held}, err
		},
	})
}

func (c *Connector) identity(id string) connector.Identity {
	kind := connector.IdentityAgent
	if c.principals[id].Kind == principal.KindHuman {
		kind = connector.IdentityUser
	}
	return connector.Identity{Source: c.source, Kind: kind, NativeID: id}
}

func (c *Connector) event(ctx context.Context, reader connector.ArtifactReader, poster, artifact string, r Request) (connector.Event, error) {
	author := c.identity(poster)
	if r.fields["author"] {
		author = c.identity(r.Author)
	}
	ev := connector.Event{Source: c.source, Time: r.CreatedAt.UTC(), Payload: connector.Payload{
		Artifact: artifact, Container: connector.Container{Kind: "project", NativeID: r.Project},
		Title: r.Name, Text: r.Description, URL: r.URL, Author: &author,
	}}
	if r.fields["author"] {
		ev.Payload.Participants = []connector.Participant{{Identity: c.identity(poster), Role: connector.RoleAgent}}
	}
	if r.State != nil {
		ev.Payload.State = &connector.State{Core: *r.State}
		if r.Status != "" {
			ev.Payload.State.Native = &r.Status
		}
	}
	ev.Kind = connector.KindIssue
	switch r.Kind {
	case "project":
		if len(r.Repos) == 0 {
			ev.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: c.source, NativeID: poster}}
			if author.NativeID != poster {
				ev.ACL = append(ev.ACL, connector.ACLEntry{Kind: connector.ACLIdentity, Source: c.source, NativeID: author.NativeID})
			}
		} else {
			refs := make([]connector.ContainerRef, 0, len(r.Repos))
			for _, repo := range r.Repos {
				refs = append(refs, connector.ContainerRef{Source: repo.Source, NativeID: repo.Project})
				ev.Payload.Touches = append(ev.Payload.Touches, connector.Touch{Source: repo.Source, Project: repo.Project})
			}
			refs, key, err := connector.CanonicalContainers(refs)
			if err != nil {
				return connector.Event{}, err
			}
			ev.ACL = connector.ACL{{Kind: connector.ACLContainers, NativeID: key, Containers: refs}}
		}
	case "workstream", "update":
		project, ok, err := reader.CurrentArtifact(ctx, c.source, r.Project)
		if err != nil {
			return connector.Event{}, fmt.Errorf("read project %s: %w", r.Project, err)
		}
		if !ok {
			return connector.Event{}, push.Refuse(push.ErrConflict, "project is not held")
		}
		ev.ACL = project.ACL
		if r.Kind == "workstream" {
			ev.Payload.PartOf = r.Project
			ev.Payload.Touches = r.Touches
		} else {
			ev.Kind = connector.KindMessage
			ev.Payload.Text = r.Text
			ev.Payload.Parent = r.Project
			if r.Workstream != "" {
				ev.Payload.Parent += "#" + r.Workstream
				if _, ok, err := reader.CurrentArtifact(ctx, c.source, ev.Payload.Parent); err != nil {
					return connector.Event{}, fmt.Errorf("read workstream: %w", err)
				} else if !ok {
					return connector.Event{}, push.Refuse(push.ErrConflict, "workstream is not held")
				}
			}
			ev.Payload.Thread = ev.Payload.Parent
		}
	}
	raw, err := json.Marshal(Native{Project: r.Project, Workstream: r.Workstream, Update: r.Update, Status: r.Status, Poster: poster})
	if err != nil {
		return connector.Event{}, fmt.Errorf("encode native ids: %w", err)
	}
	ev.Payload.Native = raw
	edited := r.UpdatedAt
	if edited.IsZero() {
		edited = r.CreatedAt
	}
	return push.Revise(ev, edited.UTC())
}

func (c *Connector) emit(ctx context.Context, sink connector.Sink, reader connector.ArtifactReader, ev connector.Event) error {
	if ev.Kind != connector.KindTombstone {
		current, ok, err := reader.CurrentArtifact(ctx, c.source, ev.Payload.Artifact)
		if err != nil {
			return fmt.Errorf("read artifact %s: %w", ev.Payload.Artifact, err)
		}
		if ok && current.ID == ev.ID {
			return nil
		}
	}
	if err := sink.Emit(ctx, ev); err != nil {
		return fmt.Errorf("emit %s: %w", ev.ID, err)
	}
	c.last.Store(time.Now().UnixNano())
	return nil
}

package tracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The request kinds.
const (
	KindTicket  = "ticket"
	KindComment = "comment"
)

// Request is one ticket or one comment, as a sender posts it. Which fields
// apply depends on Kind; a field that does not apply is refused rather than
// ignored, except on a deletion, which reads only the ids.
type Request struct {
	// Kind is `ticket` or `comment`.
	Kind string `json:"kind"`
	// Project is the project key: the container, and one of the source's
	// containers.
	Project string `json:"project"`
	// ID is the ticket's key, or the comment's id within its ticket.
	ID string `json:"id"`
	// Ticket is the key of the ticket a comment is on, in the same project.
	Ticket string `json:"ticket,omitempty"`

	// Title is a ticket's title, which it always has.
	Title string `json:"title,omitempty"`
	// Body is a ticket's description, and a comment's text, which it always
	// has.
	Body string `json:"body,omitempty"`
	// Status is the tracker's own word for where a ticket is: `open`, `In
	// Progress`, `Done`.
	Status string `json:"status,omitempty"`
	// Parent is the ticket this one is part of in the tracker's hierarchy.
	Parent *Ref `json:"parent,omitempty"`
	// Author is who wrote the ticket or the comment.
	Author *Person `json:"author,omitempty"`
	// Assignees are who a ticket is assigned to.
	Assignees []Person `json:"assignees,omitempty"`
	// URL is the page a person follows to see it in the tracker.
	URL string `json:"url,omitempty"`
	// CreatedAt is when it was created, and UpdatedAt when it last changed.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Deleted retracts the ticket or the comment.
	Deleted bool `json:"deleted,omitempty"`
	// ACL is a ticket's own access list, where the source's `ticket_acl`
	// allows one. An entry's source defaults to this source.
	ACL connector.ACL `json:"acl,omitempty"`
}

// Ref names a ticket. Project defaults to the project of the ticket naming it.
type Ref struct {
	Project string `json:"project,omitempty"`
	ID      string `json:"id"`
}

// Person is an identity hint: the tracker's own id for someone, and what else
// it knows about them. ID is what an identity mapping matches on, so it is the
// tracker's stable account id, never a name.
type Person struct {
	ID string `json:"id"`
	// Kind is `user`, the default, or `bot`.
	Kind   string `json:"kind,omitempty"`
	Handle string `json:"handle,omitempty"`
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
}

// TicketNative is what a ticket's payload.native holds.
type TicketNative struct {
	Project   string               `json:"project"`
	ID        string               `json:"id"`
	Status    string               `json:"status,omitempty"`
	Assignees []connector.Identity `json:"assignees,omitempty"`
}

// CommentNative is what a comment's payload.native holds.
type CommentNative struct {
	Project string `json:"project"`
	Ticket  string `json:"ticket"`
	ID      string `json:"id"`
}

// maxKey is the longest project key, ticket key or comment id.
const maxKey = 128

const keyGrammar = "1 to 128 bytes of letters, digits, -, _, . and /"

// validKey reports whether s may be a project key, a ticket key or a comment
// id. The set leaves out `#`, `:` and `@`, which separate the parts of an
// artifact id, and anything an event id would have to escape.
func validKey(s string) bool {
	if s == "" || len(s) > maxKey {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == '/':
		default:
			return false
		}
	}
	return true
}

// TicketArtifact is a ticket's artifact id, `<project>#<ticket>`.
func TicketArtifact(project, ticket string) string { return project + "#" + ticket }

// CommentArtifact is a comment's artifact id.
func CommentArtifact(project, ticket, comment string) string {
	return TicketArtifact(project, ticket) + ":comment:" + comment
}

// The refusals. Each is answered with its own status, and none emits anything.
var (
	// errBadRequest is a request that is not a ticket or a comment: 400.
	errBadRequest = errors.New("bad request")
	// errForbidden is a project the source does not grant: 403.
	errForbidden = errors.New("forbidden")
	// errNoTicket is a comment whose access list is its ticket's, on a ticket
	// Hearsay does not hold: 409.
	errNoTicket = errors.New("the ticket is not held")
)

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// artifact checks the ids every request carries, deletions included, and
// returns the artifact the request is about.
func (c *Connector) artifact(req Request) (string, error) {
	if !validKey(req.Project) || !validKey(req.ID) {
		return "", badRequest("project and id must each be %s", keyGrammar)
	}
	if !c.allow.Allows(c.source, req.Project) {
		return "", fmt.Errorf("%w: project %q is not one of this source's containers", errForbidden, req.Project)
	}
	switch req.Kind {
	case KindTicket:
		if req.Ticket != "" {
			return "", badRequest("a ticket names no ticket: its key is id")
		}
		return TicketArtifact(req.Project, req.ID), nil
	case KindComment:
		if !validKey(req.Ticket) {
			return "", badRequest("a comment names its ticket, %s", keyGrammar)
		}
		return CommentArtifact(req.Project, req.Ticket, req.ID), nil
	default:
		return "", badRequest("kind must be ticket or comment")
	}
}

// event turns an authenticated sender's request, which is not a deletion,
// into the event it means. The reader is where a comment's access list is
// read from when tickets carry their own.
func (c *Connector) event(ctx context.Context, reader connector.ArtifactReader, req Request) (connector.Event, error) {
	artifact, err := c.artifact(req)
	if err != nil {
		return connector.Event{}, err
	}
	switch {
	case req.CreatedAt.IsZero() || req.UpdatedAt.IsZero():
		return connector.Event{}, badRequest("created_at and updated_at are required")
	case req.UpdatedAt.Before(req.CreatedAt):
		return connector.Event{}, badRequest("updated_at is earlier than created_at")
	case req.Author == nil:
		return connector.Event{}, badRequest("author is required")
	}
	author, err := c.identity(*req.Author, "author")
	if err != nil {
		return connector.Event{}, err
	}
	if req.URL != "" {
		if u, err := url.Parse(req.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return connector.Event{}, badRequest("url must be an absolute http or https URL")
		}
	}

	ev := connector.Event{
		Source: c.source,
		Time:   req.CreatedAt.UTC(),
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: connector.Container{Kind: ContainerProject, NativeID: req.Project, Name: req.Project},
			URL:       req.URL,
			Author:    &author,
		},
	}
	var native any
	switch req.Kind {
	case KindTicket:
		if req.Title == "" {
			return connector.Event{}, badRequest("a ticket has a title")
		}
		ev.Kind = connector.KindIssue
		ev.Payload.Title, ev.Payload.Text = req.Title, req.Body
		if req.Parent != nil {
			project := req.Parent.Project
			if project == "" {
				project = req.Project
			}
			if !validKey(project) || !validKey(req.Parent.ID) {
				return connector.Event{}, badRequest("parent's project and id must each be %s", keyGrammar)
			}
			if ev.Payload.PartOf = TicketArtifact(project, req.Parent.ID); ev.Payload.PartOf == artifact {
				return connector.Event{}, badRequest("a ticket is not its own parent")
			}
		}
		t := TicketNative{Project: req.Project, ID: req.ID, Status: req.Status}
		for i, p := range req.Assignees {
			id, err := c.identity(p, fmt.Sprintf("assignees[%d]", i))
			if err != nil {
				return connector.Event{}, err
			}
			t.Assignees = append(t.Assignees, id)
		}
		native = t
		if ev.ACL, err = c.ticketAccess(req); err != nil {
			return connector.Event{}, err
		}
	case KindComment:
		switch {
		case req.Body == "":
			return connector.Event{}, badRequest("a comment has a body")
		case req.Title != "" || req.Status != "" || req.Parent != nil || req.Assignees != nil:
			return connector.Event{}, badRequest("a comment has a body and an author, and no title, status, parent or assignees")
		case req.ACL != nil:
			return connector.Event{}, badRequest("a comment has no acl of its own: it is read by whoever may read its ticket")
		}
		ev.Kind = connector.KindMessage
		ev.Payload.Text = req.Body
		ev.Payload.Parent = TicketArtifact(req.Project, req.Ticket)
		ev.Payload.Thread = ev.Payload.Parent
		native = CommentNative{Project: req.Project, Ticket: req.Ticket, ID: req.ID}
		if ev.ACL, err = c.commentAccess(ctx, reader, req.Project, ev.Payload.Parent); err != nil {
			return connector.Event{}, err
		}
	}
	if ev.Payload.Native, err = json.Marshal(native); err != nil {
		return connector.Event{}, fmt.Errorf("encoding the native object: %w", err)
	}
	return c.revise(ev, req.UpdatedAt.UTC())
}

// identity is the identity hint a person stands for, in this source.
func (c *Connector) identity(p Person, field string) (connector.Identity, error) {
	if p.ID == "" {
		return connector.Identity{}, badRequest("%s.id is required: it is the tracker's stable id for the account", field)
	}
	kind := connector.IdentityUser
	switch p.Kind {
	case "", "user":
	case "bot":
		kind = connector.IdentityBot
	default:
		return connector.Identity{}, badRequest("%s.kind must be user or bot", field)
	}
	return connector.Identity{Source: c.source, Kind: kind, NativeID: p.ID, Handle: p.Handle, DisplayName: p.Name, Email: p.Email}, nil
}

// ticketAccess is the access list of a ticket: its own, where the source lets
// it carry one and it does, and its project's otherwise.
func (c *Connector) ticketAccess(req Request) (connector.ACL, error) {
	if req.ACL == nil {
		return c.access[req.Project], nil
	}
	if !c.ticketACL {
		return nil, badRequest("this source takes no per-ticket acl: its tickets are read by whoever may read their project")
	}
	acl, err := c.qualify(req.ACL)
	if err != nil {
		return nil, badRequest("acl: %v", err)
	}
	return acl, nil
}

// commentAccess is the access list of a comment on ticket: the project's where
// tickets carry no list of their own, and the ticket's current one otherwise.
func (c *Connector) commentAccess(ctx context.Context, reader connector.ArtifactReader, project, ticket string) (connector.ACL, error) {
	if !c.ticketACL {
		return c.access[project], nil
	}
	if reader == nil {
		return nil, errors.New("the sink cannot read a ticket's access list")
	}
	current, ok, err := reader.CurrentArtifact(ctx, c.source, ticket)
	if err != nil {
		return nil, fmt.Errorf("reading ticket %s: %w", ticket, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: post ticket %s before its comments, because a comment is read by whoever may read its ticket", errNoTicket, ticket)
	}
	return current.ACL, nil
}

// revise names the observation: a hash of everything the event says, the
// time of the change and the access list included, so that the same revision
// posted again is the same event and anything else is a new one.
func (c *Connector) revise(ev connector.Event, edited time.Time) (connector.Event, error) {
	raw, err := json.Marshal(struct {
		Kind     connector.Kind    `json:"kind"`
		Time     time.Time         `json:"time"`
		EditedAt time.Time         `json:"edited_at"`
		Payload  connector.Payload `json:"payload"`
		ACL      connector.ACL     `json:"acl"`
	}{ev.Kind, ev.Time, edited, ev.Payload, ev.ACL})
	if err != nil {
		return connector.Event{}, fmt.Errorf("hashing the revision: %w", err)
	}
	sum := sha256.Sum256(raw)
	token := hex.EncodeToString(sum[:8])
	ev.NativeID = ev.Payload.Artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: edited}
	ev.ID = connector.EventID(c.source, ev.NativeID)
	return ev, nil
}

// tombstone retracts what Hearsay holds of an artifact: its current revision,
// whose container, time and access list the tombstone carries. It is named
// after that revision, so a deletion posted again is the same event, and a
// ticket deleted again after it came back is a new one. It is false when
// Hearsay holds nothing of the artifact.
func (c *Connector) tombstone(ctx context.Context, reader connector.ArtifactReader, artifact string) (connector.Event, bool, error) {
	if reader == nil {
		return connector.Event{}, false, errors.New("the sink cannot read what a deletion retracts")
	}
	current, ok, err := reader.CurrentArtifact(ctx, c.source, artifact)
	if err != nil {
		return connector.Event{}, false, fmt.Errorf("reading %s: %w", artifact, err)
	}
	if !ok {
		return connector.Event{}, false, nil
	}
	sum := sha256.Sum256([]byte(current.NativeID))
	name := artifact + ":tombstone:" + hex.EncodeToString(sum[:12])
	return connector.Event{
		ID:       connector.EventID(c.source, name),
		Source:   c.source,
		NativeID: name,
		Kind:     connector.KindTombstone,
		Time:     current.Time,
		Payload:  connector.Payload{Artifact: name, Target: artifact, Container: current.Payload.Container},
		ACL:      current.ACL,
	}, true, nil
}

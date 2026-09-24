// Package tracker is the generic tracker connector (`type: tracker`): a
// [connector.Pusher] that any issue tracker — Jira, Linear, an in-house one —
// feeds through a small sender of its own, which POSTs tickets and comments in
// a shape Hearsay defines to `/hooks/<source id>`. There is no vendor adapter
// and no pull API: the connector never calls the tracker, and a backfill is the
// sender posting what it holds again.
//
// # Configuration
//
//	# sources/tracker.yaml
//	id: linear
//	type: tracker
//	containers: [ENG, SEC]        # project keys; `*` is refused
//	settings:
//	  access:                     # required; one entry per container
//	    ENG: [{kind: public}]
//	    SEC: [{kind: group, native_id: security}]
//	  ticket_acl: false           # optional; true lets a ticket carry its own acl
//	secrets:
//	  token: HEARSAY_TRACKER_TOKEN  # required; the sender's bearer token
//
// # Access
//
// Access is the source's, per project, and it fails closed. Every project in
// `containers` needs an entry in `settings.access`, which is an access list in
// the contract's own shape; an entry's `source` defaults to this source, so a
// `group` entry is read by the members of the team that lists `{source: <this
// source>, native_id: <group>}` among its identities. A project with no entry is a startup failure,
// an entry for a project that is not a container is one too (it is a typo that
// would grant nothing), and `*` is refused because a project nobody listed has
// no access list to give its tickets.
//
// With `ticket_acl: true` a ticket may carry its own `acl`, in the same shape,
// which replaces the project's for that ticket; without it, a ticket that
// carries one is refused. A comment carries none: it inherits the access list
// of its ticket's current revision, which the connector reads from L0, and a
// comment on a ticket Hearsay does not hold is refused with 409, because
// nothing says who may read it. With `ticket_acl: false` every event of a
// project carries the project's list, so comments need no read and may arrive
// before their ticket.
//
// # What it writes
//
// A ticket is an `issue` whose artifact is `<project>#<id>` — the shape a
// scope's `tracker:` mapping reads, so item `ENG-42` of project `ENG` in source
// `linear` is entity `tracker:linear:ENG#ENG-42`. Its parent is `part_of`
// `<parent project>#<parent id>`. A comment is a `message` whose artifact is
// `<project>#<id>:comment:<comment id>`, with the ticket as `parent` and
// `thread`. The container is the project, of kind `project`.
//
// The revision token is the first 16 hex digits of the SHA-256 of what the
// event says, `updated_at` and the access list included, so posting a revision
// again is the same event id and writes nothing, and anything that changes is a
// new revision ordered by `updated_at`. A stale revision posted late is stored
// but does not become current, because an older `updated_at` orders it below
// the one L0 already has.
//
// A deletion is the ticket or comment with `deleted: true`. It retracts the
// current revision with a tombstone carrying that revision's container, time
// and access list, named after it so that deleting a ticket that came back is a
// second tombstone while a repeated deletion is the same one. Deleting what
// Hearsay does not hold emits nothing and is answered 204.
//
// The request shapes, a worked curl request and a sender sketch are the
// "Generic tracker" section of docs/connector-contract.md.
package tracker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Type is the registry key for the generic tracker connector.
const Type = "tracker"

// SecretToken is the secret the sender's bearer token is read from.
const SecretToken = "token"

// ContainerProject is the container kind of a tracker project.
const ContainerProject connector.ContainerKind = "project"

// Settings is the source's `settings`.
type Settings struct {
	// Access is the access list of each project, by project key.
	Access map[string]connector.ACL `json:"access"`
	// TicketACL lets a ticket carry an access list of its own.
	TicketACL bool `json:"ticket_acl"`
}

// Connector accepts the tickets and comments one sender posts.
type Connector struct {
	source    string
	allow     connector.Allowlist
	access    map[string]connector.ACL
	ticketACL bool
	digest    [sha256.Size]byte
	last      atomic.Int64
}

var _ connector.Pusher = (*Connector)(nil)

// Factory builds the connector for a source; it is what the binary registers
// under [Type].
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New validates a source: its token, its projects and the access list of each.
func New(src connector.SourceConfig) (*Connector, error) {
	var settings Settings
	if err := src.DecodeSettings(&settings); err != nil {
		return nil, err
	}
	for name := range src.Secrets {
		if name != SecretToken {
			return nil, fmt.Errorf("secret %q is not one a tracker source takes: it takes %q", name, SecretToken)
		}
	}
	token := src.Secrets[SecretToken]
	if token == "" {
		return nil, fmt.Errorf("secrets.%s is required: it is the bearer token the sender authenticates with", SecretToken)
	}
	if len(src.Containers) == 0 {
		return nil, errors.New("containers must name the project keys this source ingests")
	}
	c := &Connector{
		source:    src.ID,
		allow:     connector.NewAllowlist(src),
		access:    make(map[string]connector.ACL, len(src.Containers)),
		ticketACL: settings.TicketACL,
		digest:    sha256.Sum256([]byte(token)),
	}
	for _, project := range src.Containers {
		if project == connector.AllowAll {
			return nil, fmt.Errorf("containers may not be %q: every project needs its own access list in settings.access", connector.AllowAll)
		}
		if !validKey(project) {
			return nil, fmt.Errorf("container %q is not a project key: %s", project, keyGrammar)
		}
		acl, ok := settings.Access[project]
		if !ok {
			return nil, fmt.Errorf("project %q has no entry in settings.access: access fails closed, so name who may read it", project)
		}
		acl, err := c.qualify(acl)
		if err != nil {
			return nil, fmt.Errorf("settings.access.%s: %w", project, err)
		}
		c.access[project] = acl
	}
	for project := range settings.Access {
		if !slices.Contains(src.Containers, project) {
			return nil, fmt.Errorf("settings.access names project %q, which is not in containers", project)
		}
	}
	return c, nil
}

// qualify checks an access list and gives an entry with no source this one.
func (c *Connector) qualify(acl connector.ACL) (connector.ACL, error) {
	if len(acl) == 0 {
		return nil, errors.New("the access list is empty, and a ticket nobody may read cannot be read back")
	}
	out := make(connector.ACL, 0, len(acl))
	for i, entry := range acl {
		switch entry.Kind {
		case connector.ACLPublic:
			if entry.Source != "" || entry.NativeID != "" {
				return nil, fmt.Errorf("entry %d is public and names a source or a native_id", i)
			}
		case connector.ACLGroup, connector.ACLIdentity:
			if entry.Source == "" {
				entry.Source = c.source
			}
			if !connector.ValidSourceID(entry.Source) {
				return nil, fmt.Errorf("entry %d: source %q is not a source id", i, entry.Source)
			}
			if entry.NativeID == "" {
				return nil, fmt.Errorf("entry %d is a %s with no native_id", i, entry.Kind)
			}
		default:
			return nil, fmt.Errorf("entry %d: kind %q is not public, group or identity", i, entry.Kind)
		}
		out = append(out, entry)
	}
	return out, nil
}

// Describe declares tickets, comments and deletions.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindIssue, connector.KindMessage, connector.KindTombstone}}
}

// Health reports ok: a push source has nothing of its own to fail, and a quiet
// one is told from a stuck one by the time of its last event.
func (c *Connector) Health(context.Context) connector.Health {
	h := connector.Health{Status: connector.HealthOK}
	if at := c.last.Load(); at != 0 {
		h.LastEventAt = time.Unix(0, at).UTC()
	}
	return h
}

// Close releases nothing: the connector holds no connection and no goroutine.
func (c *Connector) Close(context.Context) error { return nil }

// authentic reports whether token is the source's, in constant time.
func (c *Connector) authentic(token string) bool {
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], c.digest[:]) == 1
}

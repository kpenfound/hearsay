package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
)

// The roles of a message.
const (
	// RoleOwner is the owner's own words, authored by the owner.
	RoleOwner = "owner"
	// RoleAssistant is the agent's answer, authored by the agent that posts it.
	RoleAssistant = "assistant"
)

// Request is one message, as an agent posts it.
type Request struct {
	// Owner is the owner's principal id, and must be the source's own.
	Owner string `json:"owner"`
	// Conversation is the agent's id for the conversation: the event's thread.
	Conversation string `json:"conversation"`
	// Exchange is the agent's id for the exchange within the conversation.
	Exchange string `json:"exchange"`
	// Role is `owner` or `assistant`.
	Role string `json:"role"`
	// Time is when the words were said.
	Time time.Time `json:"time"`
	// Text is the rendering of the words.
	Text string `json:"text"`
	// Interrupted marks the owner's words of an exchange that never completed.
	Interrupted bool `json:"interrupted,omitempty"`
}

// Native is what an event's payload.native holds: the parts of the request the
// standard payload fields have no place for.
type Native struct {
	Conversation string `json:"conversation"`
	Exchange     string `json:"exchange"`
	Role         string `json:"role"`
	// Interrupted is set on the owner's words of an exchange that never
	// completed, and absent otherwise.
	Interrupted bool `json:"interrupted,omitempty"`
}

// maxKey is the longest conversation or exchange id.
const maxKey = 128

const keyGrammar = "1 to 128 bytes of letters, digits, -, _ and ."

// validKey reports whether s may be a conversation or an exchange id. The set
// leaves out `/`, `:` and `@`, which separate the parts of an artifact id and
// its revision, and anything an event id would have to escape.
func validKey(s string) bool {
	if s == "" || len(s) > maxKey {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// Artifact is the artifact id of one message: `<conversation>/<exchange>:<role>`.
func Artifact(conversation, exchange, role string) string {
	return conversation + "/" + exchange + ":" + role
}

// Outcome is what storing a message did.
type Outcome string

// The outcomes of [Connector.store].
const (
	// Stored is a message, or a new revision of one, written.
	Stored Outcome = "stored"
	// Unchanged is a message whose current revision already says this: nothing
	// was written.
	Unchanged Outcome = "unchanged"
)

// ErrDeleted is what [Connector.store] returns for a message that may not be
// stored again, answered 410. Nothing is written.
var ErrDeleted = errors.New("the message was deleted")

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", push.ErrBadRequest, fmt.Sprintf(format, args...))
}

// check holds a request to the shape of a message. A body naming another owner
// is the caller's to be refused, and is reported first.
func (c *Connector) check(req Request) error {
	switch {
	case req.Owner == "":
		return badRequest("owner is required")
	case req.Owner != c.owner:
		return fmt.Errorf("%w: owner %q is not this source's owner", push.ErrForbidden, req.Owner)
	case !validKey(req.Conversation):
		return badRequest("conversation must be %s", keyGrammar)
	case !validKey(req.Exchange):
		return badRequest("exchange must be %s", keyGrammar)
	case req.Role != RoleOwner && req.Role != RoleAssistant:
		return badRequest("role must be %s or %s", RoleOwner, RoleAssistant)
	case req.Time.IsZero():
		return badRequest("time is required, as an RFC 3339 timestamp")
	case strings.TrimSpace(req.Text) == "":
		return badRequest("text is required")
	case strings.ContainsRune(req.Text, 0):
		return badRequest("text contains a NUL character")
	case req.Interrupted && req.Role != RoleOwner:
		return badRequest("interrupted is the owner's words of an exchange that never completed, so it needs role %s", RoleOwner)
	}
	return nil
}

// event is the event a checked request from agent means, as a revision of the
// message artifact after the revision named previous, which is empty for the
// first.
func (c *Connector) event(agent string, req Request, previous string) (connector.Event, error) {
	author := connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: c.owner}
	if req.Role == RoleAssistant {
		author = connector.Identity{Source: c.source, Kind: connector.IdentityAgent, NativeID: agent}
	}
	native, err := json.Marshal(Native{Conversation: req.Conversation, Exchange: req.Exchange, Role: req.Role, Interrupted: req.Interrupted})
	if err != nil {
		return connector.Event{}, fmt.Errorf("encoding the native object: %w", err)
	}
	artifact := Artifact(req.Conversation, req.Exchange, req.Role)
	ev := connector.Event{
		Source: c.source,
		Kind:   Kind,
		// Microseconds are what L0 keeps, so a message posted again compares equal
		// to the revision that was read back.
		Time: req.Time.UTC().Truncate(time.Microsecond),
		Payload: connector.Payload{
			Artifact:  artifact,
			BaseKind:  connector.KindMessage,
			Container: connector.Container{Kind: ContainerStream, NativeID: c.owner},
			Text:      req.Text,
			Author:    &author,
			Thread:    req.Conversation,
			Native:    native,
		},
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: c.source, NativeID: c.owner}},
	}
	token, err := revisionToken(ev, previous)
	if err != nil {
		return connector.Event{}, err
	}
	ev.NativeID = artifact + "@" + token
	ev.ID = connector.EventID(c.source, ev.NativeID)
	// No edited_at: the contract orders revisions that have none by arrival,
	// which is what a message posted again is.
	ev.Payload.Revision = &connector.Revision{Token: token}
	return ev, nil
}

// content is what makes one revision of a message differ from another: the
// words, the mark, when they were said, who said them and who may read them.
type content struct {
	Time        string             `json:"time"`
	Text        string             `json:"text"`
	Interrupted bool               `json:"interrupted"`
	Author      connector.Identity `json:"author"`
	ACL         connector.ACL      `json:"acl"`
}

func contentOf(ev connector.Event) (content, error) {
	var native Native
	if len(ev.Payload.Native) != 0 {
		if err := json.Unmarshal(ev.Payload.Native, &native); err != nil {
			return content{}, fmt.Errorf("reading the native object of %s: %w", ev.ID, err)
		}
	}
	out := content{Time: ev.Time.UTC().Format(time.RFC3339Nano), Text: ev.Payload.Text, Interrupted: native.Interrupted, ACL: ev.ACL}
	if ev.Payload.Author != nil {
		out.Author = *ev.Payload.Author
	}
	return out, nil
}

// revisionToken names a revision: a hash of what it says and of the revision it
// follows. Posting the same words again over the same revision is the same
// token, so it is the same event; words that come back after another revision
// are a new revision, which the same words alone would not be, so that the last
// post is always the current one.
func revisionToken(ev connector.Event, previous string) (string, error) {
	c, err := contentOf(ev)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Content  content `json:"content"`
		Previous string  `json:"previous"`
	}{c, previous})
	if err != nil {
		return "", fmt.Errorf("hashing the revision: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

// same reports whether two events say the same thing.
func same(a, b connector.Event) (bool, error) {
	x, err := contentOf(a)
	if err != nil {
		return false, err
	}
	y, err := contentOf(b)
	if err != nil {
		return false, err
	}
	return x.Time == y.Time && x.Text == y.Text && x.Interrupted == y.Interrupted && x.Author == y.Author && x.ACL.Equal(y.ACL), nil
}

// store writes a checked request from agent: the one place the source's events
// reach L0, and so the place to refuse one that may not be stored. The
// artifact is held still while it runs, so that a message deleted before the
// post, or while it waits, is never written again, and a deletion that
// follows sees what the post wrote. It writes nothing when the artifact's
// current revision already says it, and returns the event the artifact now
// stands at.
func (c *Connector) store(ctx context.Context, sink connector.Sink, agent string, req Request) (connector.Event, Outcome, error) {
	serializer, ok := sink.(connector.ArtifactSerializer)
	if !ok {
		return connector.Event{}, "", errors.New("the sink cannot hold a message still while it is written")
	}
	artifact := Artifact(req.Conversation, req.Exchange, req.Role)
	var (
		stored  connector.Event
		outcome Outcome
	)
	err := serializer.SerializeArtifact(ctx, c.source, artifact, func(ctx context.Context, held connector.LockedArtifact) error {
		if held.Deleted() {
			return ErrDeleted
		}
		current, ok := held.Current()
		previous := ""
		if ok {
			previous = current.NativeID
		}
		ev, err := c.event(agent, req, previous)
		if err != nil {
			return err
		}
		stored = ev
		if ok {
			unchanged, err := same(current, ev)
			if err != nil {
				return err
			}
			if unchanged {
				stored, outcome = current, Unchanged
				return nil
			}
		}
		if err := held.Emit(ctx, ev); err != nil {
			return err
		}
		outcome = Stored
		return nil
	})
	if err != nil {
		return stored, "", err
	}
	return stored, outcome, nil
}

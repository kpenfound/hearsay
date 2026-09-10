package l1_test

import (
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The events these tests build documents from: a small GitHub-shaped
// repository, in the shapes docs/connector-contract.md's GitHub table
// specifies.
//
// They are here rather than in a fixture file because a test that has to open a
// file to see what it is asserting about is a test nobody reads.

const (
	source = "github-acme"
	repo   = "acme/api"
)

var day = time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

// at is a time offset from the fixture day, so that ordering in a test is
// visible as a number.
func at(hours int) time.Time { return day.Add(time.Duration(hours) * time.Hour) }

// container is the repository every fixture event lives in.
var container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo, Name: repo}

// public is what a public repository's events carry.
var public = connector.ACL{{Kind: connector.ACLPublic}}

// who is an identity hint the way a GitHub connector emits one: the node id is
// what the mapping keys on, the login is what a person reads.
func who(nativeID, handle string) *connector.Identity {
	return &connector.Identity{
		Source:      source,
		Kind:        connector.IdentityUser,
		NativeID:    nativeID,
		Handle:      handle,
		DisplayName: handle,
	}
}

// agent is an identity hint for something that is not a person.
func agent(nativeID, handle string) *connector.Identity {
	id := who(nativeID, handle)
	id.Kind = connector.IdentityAgent
	return id
}

// event builds one event of an artifact that cannot change.
func event(kind connector.Kind, artifact string, when time.Time, author *connector.Identity, title, text string) connector.Event {
	return connector.Event{
		Source:   source,
		NativeID: artifact,
		Kind:     kind,
		Time:     when,
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: container,
			URL:       "https://github.com/" + repo,
			Title:     title,
			Text:      text,
			Author:    author,
		},
		ACL: public,
	}
}

// revised is one revision of an artifact that can change: the native id carries
// the source's own version token, which is what makes an edit a new event
// rather than an overwrite.
func revised(ev connector.Event, token string, editedAt time.Time) connector.Event {
	ev.NativeID = ev.Payload.Artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: editedAt}
	return ev
}

// reply hangs an event off a conversation, the way a comment or a review hangs
// off the issue or the pull request it is on.
func reply(ev connector.Event, root string) connector.Event {
	ev.Payload.Parent = root
	ev.Payload.Thread = root
	return ev
}

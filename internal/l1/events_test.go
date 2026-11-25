package l1_test

import (
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

const (
	source = "github-acme"
	repo   = "acme/api"
)

var day = time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

func at(hours int) time.Time { return day.Add(time.Duration(hours) * time.Hour) }

var container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo, Name: repo}

var public = connector.ACL{{Kind: connector.ACLPublic}}

func who(nativeID, handle string) *connector.Identity {
	return &connector.Identity{
		Source:      source,
		Kind:        connector.IdentityUser,
		NativeID:    nativeID,
		Handle:      handle,
		DisplayName: handle,
	}
}

func agent(nativeID, handle string) *connector.Identity {
	id := who(nativeID, handle)
	id.Kind = connector.IdentityAgent
	return id
}

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

func revised(ev connector.Event, token string, editedAt time.Time) connector.Event {
	ev.NativeID = ev.Payload.Artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: editedAt}
	return ev
}

func reply(ev connector.Event, root string) connector.Event {
	ev.Payload.Parent = root
	ev.Payload.Thread = root
	return ev
}

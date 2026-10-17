package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Revise names the observation: a hash of everything the event says, the
// time of the change and the access list included, so that the same revision
// posted again is the same event and anything else is a new one. Existing
// revision metadata is replaced rather than hashed into the next revision.
func Revise(ev connector.Event, edited time.Time) (connector.Event, error) {
	ev.Payload.Revision = nil
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
	ev.ID = connector.EventID(ev.Source, ev.NativeID)
	return ev, nil
}

// Tombstone retracts what Hearsay holds of an artifact: its current revision,
// whose container, time and access list the tombstone carries. It is named
// after that revision, so a deletion posted again is the same event, and a
// artifact deleted again after it came back is a new one. It is false when
// Hearsay holds nothing of the artifact.
func Tombstone(ctx context.Context, reader connector.ArtifactReader, source, artifact string) (connector.Event, bool, error) {
	if reader == nil {
		return connector.Event{}, false, errors.New("the sink cannot read what a deletion retracts")
	}
	current, ok, err := reader.CurrentArtifact(ctx, source, artifact)
	if err != nil {
		return connector.Event{}, false, fmt.Errorf("reading %s: %w", artifact, err)
	}
	if !ok {
		return connector.Event{}, false, nil
	}
	sum := sha256.Sum256([]byte(current.NativeID))
	name := artifact + ":tombstone:" + hex.EncodeToString(sum[:12])
	return connector.Event{
		ID:       connector.EventID(source, name),
		Source:   source,
		NativeID: name,
		Kind:     connector.KindTombstone,
		Time:     current.Time,
		Payload:  connector.Payload{Artifact: name, Target: artifact, Container: current.Payload.Container},
		ACL:      current.ACL,
	}, true, nil
}

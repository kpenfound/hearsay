package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
)

// Poll repairs unfinished propagation at startup and on each subsequent runtime
// pass. Re-reading under the same lock as posts keeps a startup snapshot from
// replacing a newer project's list.
func (c *Connector) Poll(ctx context.Context, sink connector.Sink) error {
	reader, ok := sink.(connector.ArtifactReader)
	if !ok {
		return errors.New("work requires an artifact reader")
	}
	all, err := reader.CurrentArtifacts(ctx, c.source)
	if err != nil {
		return fmt.Errorf("read work artifacts: %w", err)
	}
	for _, ev := range all {
		if ev.Kind != connector.KindIssue || ev.Payload.Artifact != ev.Payload.Container.NativeID {
			continue
		}
		unlock := c.lock(ev.Payload.Artifact)
		err := c.reconcile(ctx, sink, reader, ev.Payload.Artifact)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// reconcile is called only while holding the project's lock. CurrentArtifacts
// contains only live revisions, so deleting a parent never deletes its children.
func (c *Connector) reconcile(ctx context.Context, sink connector.Sink, reader connector.ArtifactReader, project string) error {
	root, held, err := reader.CurrentArtifact(ctx, c.source, project)
	if err != nil {
		return fmt.Errorf("read project %s: %w", project, err)
	}
	if !held {
		return nil
	}
	all, err := reader.CurrentArtifacts(ctx, c.source)
	if err != nil {
		return fmt.Errorf("read children of %s: %w", project, err)
	}
	for _, child := range all {
		if child.Payload.Container.NativeID != project || child.Payload.Artifact == project || child.Kind == connector.KindTombstone || child.ACL.Equal(root.ACL) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		child.ACL = root.ACL
		ev, err := push.Revise(child, child.Payload.Revision.EditedAt)
		if err != nil {
			return err
		}
		if err := c.emit(ctx, sink, reader, ev); err != nil {
			return err
		}

		current, ok, err := reader.CurrentArtifact(ctx, c.source, child.Payload.Artifact)
		if err != nil {
			return fmt.Errorf("verify child %s: %w", child.Payload.Artifact, err)
		}
		if ok && !current.ACL.Equal(root.ACL) {
			// If this ACL was served before, the first emission can deduplicate to
			// an older revision. Advance by the smallest timestamp increment above
			// the current revision to record the newly observed permission change.
			// Content and the artifact's original time remain unchanged.
			current.ACL = root.ACL
			ev, err = push.Revise(current, current.Payload.Revision.EditedAt.Add(time.Nanosecond))
			if err != nil {
				return err
			}
			if err := c.emit(ctx, sink, reader, ev); err != nil {
				return err
			}
		}
	}
	// Emission may deduplicate a revision seen earlier. Acceptance depends on the
	// lists L0 actually serves, so verify completion before answering success.
	all, err = reader.CurrentArtifacts(ctx, c.source)
	if err != nil {
		return fmt.Errorf("verify children of %s: %w", project, err)
	}
	for _, child := range all {
		if child.Payload.Container.NativeID == project && child.Payload.Artifact != project && child.Kind != connector.KindTombstone && !child.ACL.Equal(root.ACL) {
			return fmt.Errorf("project %s: access propagation to %s is incomplete", project, child.Payload.Artifact)
		}
	}
	return ctx.Err()
}

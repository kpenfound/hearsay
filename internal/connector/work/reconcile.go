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
//
// A work source repairs its own children and every owner source's records
// about its projects. An owner source repairs its own records against the
// projects its work source holds, so a pass of either completes an interrupted
// propagation to both.
func (c *Connector) Poll(ctx context.Context, sink connector.Sink) error {
	reader, ok := sink.(connector.ArtifactReader)
	if !ok {
		return errors.New("work requires an artifact reader")
	}
	c.shared.attach(c.source, sink)
	works, err := c.workReader(reader)
	if err != nil {
		return err
	}
	all, err := works.CurrentArtifacts(ctx, c.workSource())
	if err != nil {
		return fmt.Errorf("read work artifacts: %w", err)
	}
	for _, ev := range all {
		if ev.Kind != connector.KindIssue || ev.Payload.Artifact != ev.Payload.Container.NativeID {
			continue
		}
		unlock := c.lock(ev.Payload.Artifact)
		var err error
		if c.ownerOf == "" {
			err = c.reconcile(ctx, sink, reader, ev.Payload.Artifact)
		} else {
			err = c.reconcileOwned(ctx, sink, reader, works, ev.Payload.Artifact)
		}
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) reconcile(ctx context.Context, sink connector.Sink, reader connector.ArtifactReader, project string) error {
	root, held, err := reader.CurrentArtifact(ctx, c.source, project)
	if err != nil {
		return fmt.Errorf("read project %s: %w", project, err)
	}
	if !held {
		return nil
	}
	if err := c.reconcileSource(ctx, sink, reader, c.source, project, root.ACL); err != nil {
		return err
	}
	for _, owner := range c.shared.ownersOf(c.source) {
		peer, ok := c.shared.sink(owner)
		if !ok {
			return fmt.Errorf("owner source %s is not running in this process", owner)
		}
		peerReader, ok := peer.(connector.ArtifactReader)
		if !ok {
			return fmt.Errorf("owner source %s cannot be read", owner)
		}
		if err := c.reconcileSource(ctx, peer, peerReader, owner, project, root.ACL); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (c *Connector) reconcileOwned(ctx context.Context, sink connector.Sink, reader, works connector.ArtifactReader, project string) error {
	root, held, err := works.CurrentArtifact(ctx, c.ownerOf, project)
	if err != nil {
		return fmt.Errorf("read project %s: %w", project, err)
	}
	if !held {
		return nil
	}
	return c.reconcileSource(ctx, sink, reader, c.source, project, root.ACL)
}

func (c *Connector) reconcileSource(ctx context.Context, sink connector.Sink, reader connector.ArtifactReader, source, project string, acl connector.ACL) error {
	all, err := reader.CurrentArtifacts(ctx, source)
	if err != nil {
		return fmt.Errorf("read children of %s: %w", project, err)
	}
	for _, child := range all {
		if child.Payload.Container.NativeID != project || child.Payload.Artifact == project || child.Kind == connector.KindTombstone || child.ACL.Equal(acl) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		child.ACL = acl
		ev, err := push.Revise(child, child.Payload.Revision.EditedAt)
		if err != nil {
			return err
		}
		if err := c.emit(ctx, sink, reader, ev); err != nil {
			return err
		}

		current, ok, err := reader.CurrentArtifact(ctx, source, child.Payload.Artifact)
		if err != nil {
			return fmt.Errorf("verify child %s: %w", child.Payload.Artifact, err)
		}
		if ok && !current.ACL.Equal(acl) {
			current.ACL = acl
			ev, err = push.Revise(current, current.Payload.Revision.EditedAt.Add(time.Nanosecond))
			if err != nil {
				return err
			}
			if err := c.emit(ctx, sink, reader, ev); err != nil {
				return err
			}
		}
	}
	all, err = reader.CurrentArtifacts(ctx, source)
	if err != nil {
		return fmt.Errorf("verify children of %s: %w", project, err)
	}
	for _, child := range all {
		if child.Payload.Container.NativeID == project && child.Payload.Artifact != project && child.Kind != connector.KindTombstone && !child.ACL.Equal(acl) {
			return fmt.Errorf("project %s: access propagation to %s is incomplete", project, child.Payload.Artifact)
		}
	}
	return ctx.Err()
}

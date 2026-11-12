package l1

import (
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
)

// WorkSourceOf is the work source whose entities a `work` source's records are
// about: the source itself, or the work source it is the owner source of. It
// is false for a source that is not a `work` source.
func WorkSourceOf(repo config.Repo, source string) (string, bool) {
	cfg, ok := repo.Source(source)
	if !ok || cfg.Type != work.Type {
		return "", false
	}
	if owned := work.OwnerOf(cfg); owned != "" {
		return owned, true
	}
	return source, true
}

// workScope is the `work:` entities a record of a `work` source is about: its
// project's and, below a workstream, the workstream's, named after the work
// source that holds the project. An owner source's documents and decisions are
// about its work source's entities, so one read of the entity serves both
// sources' records.
//
// A document or decision names its project and workstream in its structured
// fields, because its artifact id carries the record's own id after them. A
// project, workstream or update is named by its artifact id, and an update is a
// message in its project's or workstream's document, so the same scope covers
// its content without an update entity.
func workScope(root connector.Event, repo config.Repo) []string {
	ws, ok := WorkSourceOf(repo, root.Source)
	if !ok {
		return nil
	}
	if held, ok := work.Decode(root); ok {
		ids := []string{"work:" + ws + ":" + held.Project}
		if held.Workstream != "" {
			ids = append(ids, "work:"+ws+":"+held.Project+"#"+held.Workstream)
		}
		return ids
	}
	return []string{
		"work:" + ws + ":" + root.Payload.Container.NativeID,
		"work:" + ws + ":" + root.Payload.Artifact,
	}
}

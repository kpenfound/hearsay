package l2

import (
	"context"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// HierarchySource is one of the places a `part_of` edge comes from. They are
// ranked (ADR-0016): configuration outranks tracker hierarchy, which outranks
// repository structure.
type HierarchySource string

// The hierarchy sources, highest rank first.
const (
	HierarchyConfig        HierarchySource = "config"
	HierarchyTracker       HierarchySource = "tracker"
	HierarchyRepoStructure HierarchySource = "repo_structure"
)

// Parents is what one hierarchy source says: the parents it sets per entity id.
type Parents map[string][]string

// HierarchyInputs is every source's parents, one field per source.
type HierarchyInputs struct {
	// Config is `code/`'s part_of, which people write. The loader refuses a
	// cycle in it.
	Config Parents
	// Tracker is the tracker's hierarchy: sub-issues under their parents.
	Tracker Parents
	// RepoStructure is where a code entity sits in its repository
	// ([NestByPattern]).
	RepoStructure Parents
}

// MergeHierarchy is the `part_of` edges of every entity any source sets a
// parent for (ADR-0016).
//
// For each entity, the highest-ranked source that sets any parent is the one
// its parents come from: they replace every lower source's, and are never
// added to them. Edges are then taken highest rank first, and within one rank
// by entity id and in the order the source lists them. An edge that would
// close a cycle with the edges already taken is dropped and logged; so is an
// entity named as its own parent. A dropped edge is not replaced by a
// lower-ranked source's.
//
// It depends only on its inputs. An entity whose every edge was dropped is
// left out of the result.
func MergeHierarchy(ctx context.Context, in HierarchyInputs) Parents {
	ranked := []struct {
		source  HierarchySource
		parents Parents
	}{
		{HierarchyConfig, in.Config},
		{HierarchyTracker, in.Tracker},
		{HierarchyRepoStructure, in.RepoStructure},
	}
	// The source each entity takes its parents from.
	chosen := map[string]HierarchySource{}
	for _, r := range ranked {
		for id, parents := range r.parents {
			if _, ok := chosen[id]; !ok && len(parents) > 0 {
				chosen[id] = r.source
			}
		}
	}

	log := telemetry.Logger(ctx)
	out := Parents{}
	for _, r := range ranked {
		for _, id := range sortedKeys(r.parents) {
			if chosen[id] != r.source {
				continue
			}
			for _, parent := range r.parents[id] {
				if slices.Contains(out[id], parent) {
					continue
				}
				if parent == id || reaches(out, parent, id) {
					log.WarnContext(ctx, "dropped a part_of edge that would close a cycle in the entity hierarchy",
						"entity", id, "parent", parent, "source", string(r.source))
					continue
				}
				out[id] = append(out[id], parent)
			}
		}
	}
	return out
}

// reaches reports whether the walk up part_of from one entity arrives at
// another.
func reaches(parents Parents, from, to string) bool {
	visited := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == to {
			return true
		}
		if visited[id] {
			continue
		}
		visited[id] = true
		stack = append(stack, parents[id]...)
	}
	return false
}

// Located is a code entity's place in a repository, which is all
// [NestByPattern] reads.
type Located struct {
	ID           string
	Repo         config.SourceRef
	PathPatterns []string
}

// NestByPattern is the hierarchy repository structure implies, read from path
// patterns alone: patterns are never expanded into file lists, and no
// repository is read.
//
// Within one repository, an entity whose patterns all lie under another
// entity's is part of the nearest such entity: the one whose covering patterns
// are deepest, or every one tied for deepest. A pattern lies under a pattern of
// the form `dir/**` when its fixed directory prefix is `dir` or below it, and
// under nothing else: `**/*.go` covers a great deal, but no directory, so it
// is nobody's parent. Two entities whose patterns cover each other's cover the
// same paths, and are not nested in each other.
//
// An entity with patterns that lies under nobody's is part of the
// repository's project, `code:<owner>/<repo>`, when that entity is among the
// ones given. An entity with no repository or no patterns has no parent here.
func NestByPattern(entities []Located) Parents {
	out := Parents{}
	for _, child := range entities {
		if child.Repo.Source == "" || len(child.PathPatterns) == 0 {
			continue
		}
		best, nearest := -1, []string(nil)
		for _, parent := range entities {
			if parent.ID == child.ID || parent.Repo != child.Repo {
				continue
			}
			depth, ok := covers(parent.PathPatterns, child.PathPatterns)
			if _, back := covers(child.PathPatterns, parent.PathPatterns); back {
				// The two cover the same paths: neither is inside the other.
				ok = false
			}
			switch {
			case !ok || depth < best:
			case depth > best:
				best, nearest = depth, []string{parent.ID}
			default:
				nearest = append(nearest, parent.ID)
			}
		}
		if nearest == nil {
			project := "code:" + child.Repo.Project
			if project != child.ID && slices.ContainsFunc(entities, func(e Located) bool {
				return e.ID == project && e.Repo == child.Repo
			}) {
				nearest = []string{project}
			}
		}
		if nearest != nil {
			slices.Sort(nearest)
			out[child.ID] = nearest
		}
	}
	return out
}

// covers reports whether every child pattern lies under one of the parent's,
// and how deep the covering is: the sum, over the child's patterns, of the
// segments in the deepest parent directory covering each.
func covers(parent, child []string) (int, bool) {
	total := 0
	for _, c := range child {
		deepest := -1
		for _, p := range parent {
			dir, ok := subtree(p)
			if !ok || !under(c, dir) {
				continue
			}
			deepest = max(deepest, strings.Count(dir, "/")+1)
		}
		if deepest < 0 {
			return 0, false
		}
		total += deepest
	}
	return total, true
}

// subtree is the directory a pattern of the form `dir/**` names, and whether
// the pattern is of that form.
func subtree(pattern string) (string, bool) {
	pattern = strings.TrimPrefix(strings.TrimPrefix(pattern, "./"), "/")
	dir, ok := strings.CutSuffix(pattern, "/**")
	if !ok || dir == "" || strings.ContainsAny(dir, "*?[") {
		return "", false
	}
	return dir, true
}

// under reports whether every path a pattern matches is inside a directory.
func under(pattern, dir string) bool {
	prefix := staticPrefix(pattern)
	return prefix == dir || strings.HasPrefix(prefix, dir+"/")
}

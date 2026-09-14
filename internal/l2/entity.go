package l2

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
)

// ErrInvalid is wrapped by everything this package refuses to store, so a
// caller can tell a malformed entity, topic or stance from an IO error.
var ErrInvalid = errors.New("invalid l2 object")

// ErrNotFound is an id the graph does not hold.
var ErrNotFound = errors.New("no such l2 object")

// EntityType is what sort of thing an entity is. The set is the design's
// (docs/design.md#entities), and the column carries a CHECK over exactly it.
type EntityType string

// The entity types.
const (
	TypeModule      EntityType = "module"
	TypeService     EntityType = "service"
	TypeSymbol      EntityType = "symbol"
	TypeTrackerItem EntityType = "tracker_item"
	TypeProject     EntityType = "project"
	TypePerson      EntityType = "person"
	TypeTeam        EntityType = "team"
)

// Valid reports whether t is one of the design's entity types.
func (t EntityType) Valid() bool {
	switch t {
	case TypeModule, TypeService, TypeSymbol, TypeTrackerItem, TypeProject, TypePerson, TypeTeam:
		return true
	}
	return false
}

// Origin is where an entity came from. A re-seed replaces what it seeded — the
// `config` and `repo_structure` rows — and never touches a `reference` row,
// which the assertion worker created because a document pointed at it.
type Origin string

// The origins.
const (
	OriginConfig        Origin = "config"
	OriginRepoStructure Origin = "repo_structure"
	OriginReference     Origin = "reference"
)

// Valid reports whether o is one of the three.
func (o Origin) Valid() bool {
	return o == OriginConfig || o == OriginRepoStructure || o == OriginReference
}

// Entity is anything documents are about (docs/design.md#entities).
type Entity struct {
	ID   string
	Type EntityType
	// Name is the display name, and resolve matches it the way it matches an
	// alias.
	Name    string
	Aliases []string
	// PathPatterns locate a code entity in its repository, resolved against
	// HEAD at read time. Patterns, never file lists.
	PathPatterns []string
	// PartOf is the hierarchy: the ids of the entities this one is part of.
	PartOf []string
	// Owners are principal ids.
	Owners []string
	Origin Origin
}

// Validate reports an entity the store refuses. Every rule is one the table
// also enforces.
func (e Entity) Validate() error {
	switch {
	case e.ID == "":
		return fmt.Errorf("%w: an entity has no id", ErrInvalid)
	case !e.Type.Valid():
		return fmt.Errorf("%w: entity %s has type %q", ErrInvalid, e.ID, e.Type)
	case !e.Origin.Valid():
		return fmt.Errorf("%w: entity %s has origin %q", ErrInvalid, e.ID, e.Origin)
	}
	return nil
}

// RepoReader reads what seeding needs out of a repository: one file, and the
// names of the directories at its root. Hearsay holds no code, so this is the
// whole of what it ever reads from one (docs/design.md#entities).
//
// Nothing in this build implements it against a live source — the GitHub
// connector will (#8) — so a process without one seeds from `code/`
// configuration alone and says so.
type RepoReader interface {
	// ReadFile returns a file's content at HEAD.
	ReadFile(ctx context.Context, repo config.SourceRef, path string) ([]byte, error)
	// TopLevel returns the names of the directories at the root, at HEAD.
	TopLevel(ctx context.Context, repo config.SourceRef) ([]string, error)
}

// Seed is the entity map a configuration describes: every `code/` entry, a
// project and a module per top-level directory of every repository those
// entries name, and owners imported from each entry's CODEOWNERS file.
//
// Configuration wins every disagreement. A top-level directory whose id `code/`
// already declares is not seeded a second time, and an entity with owners
// configured keeps them rather than taking the file's. With a nil reader only
// the `code/` entries are returned, because the other two sources need a
// repository to read.
//
// The result is sorted by id and depends only on its inputs, so seeding twice
// writes nothing new.
func Seed(ctx context.Context, repo config.Repo, reader RepoReader) ([]Entity, error) {
	byID := map[string]*Entity{}
	for _, c := range repo.Code {
		byID[c.ID] = &Entity{
			ID:           c.ID,
			Type:         EntityType(c.Type),
			Name:         c.Name,
			Aliases:      slices.Clone(c.Aliases),
			PathPatterns: slices.Clone(c.PathPatterns),
			PartOf:       slices.Clone(c.PartOf),
			Owners:       slices.Clone(c.Owners),
			Origin:       OriginConfig,
		}
	}
	if reader == nil {
		return sorted(byID), nil
	}

	for _, ref := range repositories(repo.Code) {
		dirs, err := reader.TopLevel(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("reading the top level of %s in %s: %w", ref.Project, ref.Source, err)
		}
		project := "code:" + ref.Project
		if _, ok := byID[project]; !ok {
			byID[project] = &Entity{ID: project, Type: TypeProject, Name: ref.Project, Origin: OriginRepoStructure}
		}
		for _, dir := range dirs {
			dir = strings.Trim(dir, "/")
			if dir == "" || strings.Contains(dir, "/") || strings.HasPrefix(dir, ".") {
				// Not a top-level directory, or a dot-directory (.github,
				// .dagger), which is tooling rather than something a team
				// talks about as part of the product.
				continue
			}
			id := project + ":" + dir
			if _, ok := byID[id]; ok {
				continue
			}
			byID[id] = &Entity{
				ID: id, Type: TypeModule, Name: dir,
				PathPatterns: []string{dir + "/**"},
				PartOf:       []string{project},
				Origin:       OriginRepoStructure,
			}
		}
	}

	resolver, err := repo.Resolver()
	if err != nil {
		return nil, fmt.Errorf("building the identity resolver: %w", err)
	}
	for _, c := range repo.Code {
		if c.CodeOwners == "" || c.Repo.Source == "" {
			continue
		}
		content, err := reader.ReadFile(ctx, c.Repo, c.CodeOwners)
		if err != nil {
			return nil, fmt.Errorf("reading %s for %s: %w", c.CodeOwners, c.ID, err)
		}
		rules := ParseCodeOwners(content)
		// The file seeds owners for the entity that names it and for everything
		// under it (docs/config.md#code): the descendants share its repository.
		for _, e := range descendants(byID, c.ID) {
			// Owners already set are configuration's: nothing else has set any
			// by this point.
			if len(e.Owners) > 0 {
				continue
			}
			var spellings []string
			for _, pattern := range e.PathPatterns {
				spellings = append(spellings, OwnersOf(rules, staticPrefix(pattern))...)
			}
			e.Owners = sortedUnique(ResolveOwners(resolver, c.Repo.Source, spellings))
		}
	}
	return sorted(byID), nil
}

// repositories is every repository `code/` names, once each, sorted.
func repositories(code []config.CodeEntity) []config.SourceRef {
	var refs []config.SourceRef
	for _, c := range code {
		if c.Repo.Source == "" || slices.Contains(refs, c.Repo) {
			continue
		}
		refs = append(refs, c.Repo)
	}
	slices.SortFunc(refs, func(a, b config.SourceRef) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Project, b.Project))
	})
	return refs
}

// descendants is the entity and everything whose part_of chain reaches it. The
// loader refuses a cycle in `code/`, and seeded entities only point at a
// project, so the walk ends; the visited set is what keeps it ending anyway.
func descendants(byID map[string]*Entity, root string) []*Entity {
	visited := map[string]bool{}
	var out []*Entity
	var walk func(id string)
	walk = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
		for _, childID := range sortedKeys(byID) {
			if slices.Contains(byID[childID].PartOf, id) {
				walk(childID)
			}
		}
	}
	walk(root)
	return out
}

func sorted(byID map[string]*Entity) []Entity {
	out := make([]Entity, 0, len(byID))
	for _, id := range sortedKeys(byID) {
		out = append(out, *byID[id])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func sortedUnique(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

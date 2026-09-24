package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
)

type identityEntry struct {
	Source      string   `json:"source"`
	NativeID    string   `json:"native_id"`
	Handle      string   `json:"handle,omitempty"`
	Email       string   `json:"email,omitempty"`
	Name        string   `json:"name,omitempty"`
	Status      string   `json:"status"`
	Count       int      `json:"count"`
	Candidates  []string `json:"candidates"`
	Suggestions []string `json:"suggestions"`
}

type identitySource struct {
	Source     string          `json:"source"`
	Identities []identityEntry `json:"identities"`
}

func runIdentities(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("identities", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	asJSON := fs.Bool("json", false, "print stable machine-readable JSON")
	asYAML := fs.Bool("yaml", false, "print principals with identities to paste into config")
	action, words, err := parseAction(fs, args, "")
	if err != nil {
		return err
	}
	if action != "list" {
		return fmt.Errorf("unknown action %q: want list", action)
	}
	if err := checkArgs(fs, action, words, "config", "json", "yaml"); err != nil {
		return err
	}
	if *asJSON && *asYAML {
		return errors.New("--json and --yaml cannot be combined")
	}
	if *configPath == "" {
		return errors.New("identities list needs --config")
	}
	resolveDatabase()
	if cfg.Database.URL == "" {
		return errors.New("identities list needs --database-url or HEARSAY_DATABASE_URL")
	}
	repo, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	resolver, err := principal.NewResolver(repo.Principals)
	if err != nil {
		return err
	}
	ctx, err = withLogger(ctx, "identities", cfg, stderr)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	groups, err := collectIdentities(ctx, l0.New(pool), resolver, repo.Principals)
	if err != nil {
		return err
	}
	switch {
	case *asJSON:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(groups)
	case *asYAML:
		return printIdentityYAML(stdout, groups)
	default:
		printIdentityText(stdout, groups)
		return nil
	}
}

type hintReader interface {
	IdentityHints(context.Context, func(connector.Identity) error) error
}

func collectIdentities(ctx context.Context, reader hintReader, resolver *principal.Resolver, configured []principal.Principal) ([]identitySource, error) {
	entries := map[string]*identityEntry{}
	err := reader.IdentityHints(ctx, func(h connector.Identity) error {
		if h.Kind == connector.IdentityBot || h.Source == connector.SelfSource || h.Source == "" || h.NativeID == "" {
			return nil
		}
		res := resolver.Resolve(h)
		if res.Status == principal.Resolved {
			return nil
		}
		key := h.Source + "\x00" + h.NativeID
		e := entries[key]
		if e == nil {
			e = &identityEntry{Source: h.Source, NativeID: h.NativeID}
			entries[key] = e
		}
		e.Handle, e.Email, e.Name = h.Handle, h.Email, h.DisplayName
		e.Status = res.Status.String()
		e.Count++
		e.Candidates = append([]string{}, res.Candidates...)
		e.Suggestions = suggestPrincipals(h, configured, e.Candidates)
		return nil
	})
	if err != nil {
		return nil, err
	}
	bySource := map[string][]identityEntry{}
	for _, e := range entries {
		bySource[e.Source] = append(bySource[e.Source], *e)
	}
	sources := make([]string, 0, len(bySource))
	for source := range bySource {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	out := make([]identitySource, 0, len(sources))
	for _, source := range sources {
		list := bySource[source]
		slices.SortFunc(list, func(a, b identityEntry) int {
			if a.Count != b.Count {
				if a.Count > b.Count {
					return -1
				}
				return 1
			}
			return strings.Compare(a.NativeID, b.NativeID)
		})
		out = append(out, identitySource{Source: source, Identities: list})
	}
	return out, nil
}

func suggestPrincipals(h connector.Identity, configured []principal.Principal, candidates []string) []string {
	match := map[string]bool{}
	for _, c := range candidates {
		match[c] = true
	}
	for _, p := range configured {
		for _, value := range []string{p.ID, p.Name} {
			if value != "" && (principal.FoldHandle(value) == principal.FoldHandle(h.Handle) || principal.FoldHandle(value) == principal.FoldHandle(h.DisplayName) || principal.FoldHandle(value) == principal.FoldHandle(h.Email)) {
				match[p.ID] = true
			}
		}
		for _, id := range p.Identities {
			for _, value := range []string{id.Handle} {
				if value != "" && (principal.FoldHandle(value) == principal.FoldHandle(h.Handle) || principal.FoldHandle(value) == principal.FoldHandle(h.Email)) {
					match[p.ID] = true
				}
			}
		}
	}
	out := make([]string, 0, len(match))
	for id := range match {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func printIdentityText(w io.Writer, groups []identitySource) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "SOURCE\tCOUNT\tSTATUS\tNATIVE ID\tHANDLE\tEMAIL\tSUGGESTIONS\n")
	for _, group := range groups {
		for _, e := range group.Identities {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", e.Source, e.Count, e.Status, e.NativeID, e.Handle, e.Email, strings.Join(e.Suggestions, ", "))
		}
	}
	_ = tw.Flush()
}

func printIdentityYAML(w io.Writer, groups []identitySource) error {
	type mapping struct {
		Source   string `yaml:"source"`
		NativeID string `yaml:"native_id"`
		Handle   string `yaml:"handle,omitempty"`
	}
	type entry struct {
		ID         string    `yaml:"id"`
		Identities []mapping `yaml:"identities"`
	}
	type document struct {
		Principals []entry `yaml:"principals"`
	}
	byID := map[string][]mapping{}
	for _, group := range groups {
		for _, e := range group.Identities {
			id := ""
			if len(e.Suggestions) == 1 {
				id = e.Suggestions[0]
			}
			if id == "" {
				id = fmt.Sprintf("new-%.16x", sha256.Sum256([]byte(e.Source+"\x00"+e.NativeID)))
			}
			handle := e.Handle
			if handle == "" {
				handle = e.Email
			}
			byID[id] = append(byID[id], mapping{Source: e.Source, NativeID: e.NativeID, Handle: handle})
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	doc := document{Principals: make([]entry, 0, len(ids))}
	for _, id := range ids {
		doc.Principals = append(doc.Principals, entry{ID: id, Identities: byID[id]})
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return enc.Close()
}

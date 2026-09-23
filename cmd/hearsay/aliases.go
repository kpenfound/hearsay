package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

// runAliases lets a named human inspect and decide only candidates whose
// current evidence access lists allow that human to read them.
func runAliases(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("aliases", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	who := fs.String("principal", "", "configured human principal making the decision")
	entity := fs.String("entity", "", "for list: only this entity")
	action, words, err := parseAction(fs, args, "")
	if err != nil {
		return err
	}
	switch action {
	case "list":
		if err := checkArgs(fs, action, words, "config", "principal", "entity"); err != nil {
			return err
		}
	case "confirm", "reject":
		if err := checkFlags(fs, action, "config", "principal"); err != nil {
			return err
		}
		if len(words) != 2 {
			return fmt.Errorf("%s takes an entity id and an alias name", action)
		}
	default:
		return errors.New("unknown action: want list, confirm or reject")
	}
	if *configPath == "" || *who == "" {
		return errors.New("aliases needs --config and --principal")
	}
	resolveDatabase()
	repo, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	human, ok := repo.Principal(*who)
	if !ok {
		return errors.New("unknown principal")
	}
	eff, err := principal.HumanRead(human, principal.Grant{Scopes: principal.AllScopes()})
	if err != nil {
		return err
	}
	resolver, err := repo.Resolver()
	if err != nil {
		return err
	}
	reader, err := l1.ReaderFor(resolver, eff)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := l2.New(pool)
	if action == "list" {
		candidates, err := store.AliasCandidates(ctx, *entity)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, c := range candidates {
			if reader.Allows(c.ACL) {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", c.EntityID, c.Name, c.State, c.Votes)
			}
		}
		return tw.Flush()
	}
	candidate, err := store.AliasCandidate(ctx, words[0], words[1])
	if err != nil || !reader.Allows(candidate.ACL) {
		return errors.New("alias candidate unavailable")
	}
	state := "confirmed"
	if action == "reject" {
		state = "rejected"
	}
	if err := store.SetAliasState(ctx, words[0], words[1], state); err != nil {
		return err
	}
	fmt.Fprintln(stdout, state)
	return nil
}

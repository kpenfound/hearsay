package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/deletion"
)

func runDelete(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("delete", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	event := fs.String("event", "", "L0 event id")
	author := fs.String("author", "", "configured principal or source:native-id")
	reason := fs.String("reason", "", "reason for deletion (required, preview or not)")
	asJSON := fs.Bool("json", false, "print the preview, what was applied, or the deletion records as JSON")
	apply := fs.Bool("apply", false, "delete: redact the covered L0 events and re-distill what depended on them")
	operator := fs.String("principal", "", "with --apply: the configured human principal applying the deletion")
	// flag.FlagSet consumes one value, whereas --artifact has two. Extract its
	// pair first and let parseWords handle all remaining flags in any position.
	var artifactSource, artifactID string
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--artifact" || args[i] == "-artifact" {
			if artifactSource != "" {
				return errors.New("--artifact given more than once")
			}
			if i+2 >= len(args) || strings.HasPrefix(args[i+1], "-") || strings.HasPrefix(args[i+2], "-") {
				return errors.New("--artifact needs <source> <artifact-id>")
			}
			artifactSource, artifactID = args[i+1], args[i+2]
			i += 2
			continue
		}
		filtered = append(filtered, args[i])
	}
	words, err := parseWords(fs, filtered)
	if err != nil {
		return err
	}
	if len(words) != 0 {
		if words[0] != "list" && words[0] != "show" {
			return fmt.Errorf("unexpected argument %q", words[0])
		}
		if artifactSource != "" {
			return fmt.Errorf("delete %s does not read --artifact", words[0])
		}
		return runDeleteRecord(ctx, fs, cfg, resolveDatabase, words, *asJSON, stdout)
	}
	if *reason == "" {
		return errors.New("--reason is required")
	}
	if *configPath == "" && *author != "" && !strings.Contains(*author, ":") {
		return errors.New("--author requires --config")
	}
	// Who applies a deletion is settled before anything is opened: the
	// operator is a configured human, named, and never inferred.
	if !*apply && *operator != "" {
		return errors.New("--principal is only read with --apply: a preview needs no operator")
	}
	if *apply && (*configPath == "" || *operator == "") {
		return errors.New("--apply needs --config and --principal <human-id>")
	}
	var repo config.Repo
	if *configPath != "" {
		repo, err = config.Load(*configPath)
		if err != nil {
			return err
		}
	}
	if *apply {
		if err := deletion.CheckOperator(repo, *operator); err != nil {
			return err
		}
	}
	resolveDatabase()
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	sel := deletion.Selector{Event: *event, ArtifactSource: artifactSource, ArtifactID: artifactID, Author: *author}
	if *apply {
		applied, err := deletion.Apply(ctx, pool, repo, sel, *reason, *operator)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(applied)
		}
		fmt.Fprintf(stdout, "Deletion %s applied (%s) by %s\nReason: %s\n", applied.Deletion, selectorLabel(sel), applied.Operator, *reason)
		printIDs(stdout, "L0 events redacted", applied.Events)
		printIDs(stdout, "L1 documents queued for re-distillation", applied.Documents)
		return nil
	}
	p, err := deletion.Walk(ctx, pool, repo, sel, *reason)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(struct {
			deletion.Preview
			Counts map[string]int `json:"counts"`
		}{p, p.Counts()})
	}
	fmt.Fprintf(stdout, "Deletion preview (%s)\nReason: %s\n", selectorLabel(p.Selector), p.Reason)
	for _, layer := range []struct {
		name string
		ids  []string
	}{
		{"L0 events", p.Events}, {"L1 documents", p.Documents}, {"L2 stances", p.Stances},
		{"L2 topics", p.Topics}, {"Alias candidates", p.AliasCandidates}, {"Pins", p.Pins},
	} {
		printIDs(stdout, layer.name, layer.ids)
	}
	return nil
}

func printIDs(w io.Writer, name string, ids []string) {
	fmt.Fprintf(w, "%s (%d):\n", name, len(ids))
	for _, id := range ids {
		fmt.Fprintf(w, "  %s\n", id)
	}
}

func selectorLabel(s deletion.Selector) string {
	if s.Event != "" {
		return "event " + s.Event
	}
	if s.Author != "" {
		return "author " + s.Author
	}
	return "artifact " + s.ArtifactSource + " " + s.ArtifactID
}

// runDeleteRecord is `hearsay delete list` and `hearsay delete show <id>`: the
// audit trail of deletions applied, read-only.
func runDeleteRecord(ctx context.Context, fs *flag.FlagSet, cfg *config.Config, resolveDatabase func(), words []string, asJSON bool, stdout io.Writer) error {
	action := "delete " + words[0]
	if err := checkFlags(fs, action, "json"); err != nil {
		return err
	}
	switch {
	case words[0] == "list" && len(words) > 1:
		return fmt.Errorf("unexpected argument %q: %s takes none", words[1], action)
	case words[0] == "show" && len(words) != 2:
		return errors.New("delete show needs exactly one deletion id")
	}
	resolveDatabase()
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if words[0] == "list" {
		deletions, err := deletion.List(ctx, pool)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(stdout).Encode(deletions)
		}
		return printDeletions(stdout, deletions)
	}
	rec, err := deletion.Show(ctx, pool, words[1])
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(rec)
	}
	printDeletion(stdout, rec)
	return nil
}

func printDeletions(w io.Writer, deletions []deletion.Summary) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tOPERATOR\tSTATUS\tSELECTOR\tREASON")
	for _, d := range deletions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Time.Format(time.RFC3339), d.Operator, d.Status, selectorLabel(d.Selector), d.Reason)
	}
	return tw.Flush()
}

func printDeletion(w io.Writer, rec deletion.Record) {
	fmt.Fprintf(w, "Deletion %s (%s)\n", rec.ID, selectorLabel(rec.Selector))
	fmt.Fprintf(w, "Time: %s\nOperator: %s\nReason: %s\nStatus: %s\n", rec.Time.Format(time.RFC3339), rec.Operator, rec.Reason, rec.Status)
	fmt.Fprintf(w, "Retraction event: %s\nReplays dropped: %d\n", rec.Retraction, rec.Replays)
	printIDs(w, "L0 events redacted", rec.Events)
	fmt.Fprintf(w, "L1 documents rebuilt (%d):\n", len(rec.Documents))
	for _, d := range rec.Documents {
		at := ""
		if d.At != nil {
			at = " at " + d.At.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "  %s %s%s\n", d.ID, d.Outcome, at)
	}
	printIDs(w, "L2 stances superseded, position redacted", rec.Stances)
	printIDs(w, "L2 topics, name redacted", rec.Topics)
}

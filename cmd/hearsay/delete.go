package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/deletion"
)

func runDelete(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("delete", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	event := fs.String("event", "", "L0 event id")
	author := fs.String("author", "", "configured principal or source:native-id")
	reason := fs.String("reason", "", "reason for deletion (required for preview)")
	asJSON := fs.Bool("json", false, "print a JSON preview")
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
		return fmt.Errorf("unexpected argument %q", words[0])
	}
	if *reason == "" {
		return errors.New("--reason is required")
	}
	if *configPath == "" && *author != "" && !strings.Contains(*author, ":") {
		return errors.New("--author requires --config")
	}
	var repo config.Repo
	if *configPath != "" {
		repo, err = config.Load(*configPath)
		if err != nil {
			return err
		}
	}
	resolveDatabase()
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	p, err := deletion.Walk(ctx, pool, repo, deletion.Selector{Event: *event, ArtifactSource: artifactSource, ArtifactID: artifactID, Author: *author}, *reason)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(p)
	}
	fmt.Fprintf(stdout, "Deletion preview (%s)\nReason: %s\n", selectorLabel(p.Selector), p.Reason)
	for _, layer := range []struct {
		name string
		ids  []string
	}{
		{"L0 events", p.Events}, {"L1 documents", p.Documents}, {"L2 stances", p.Stances},
		{"L2 topics", p.Topics}, {"Alias candidates", p.AliasCandidates}, {"Pins", p.Pins},
	} {
		fmt.Fprintf(stdout, "%s (%d):\n", layer.name, len(layer.ids))
		for _, id := range layer.ids {
			fmt.Fprintf(stdout, "  %s\n", id)
		}
	}
	return nil
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

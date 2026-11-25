package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

func runL0(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, _ := newFlagSet("l0", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	var (
		source   = fs.String("source", "", "for list and tail: only events from this configured source")
		kind     = fs.String("kind", "", "for list and tail: only events of this kind")
		artifact = fs.String("artifact", "", "for list and tail: only this artifact's history; needs --source")
		limit    = fs.Int("limit", 0, "for list and tail: how many events to return (default 100, maximum 1000)")
		newest   = fs.Bool("newest", false, "for list: newest first, which for one artifact is the contract's revision order")
		cursor   = fs.String("cursor", "", "for tail: resume after this cursor instead of the beginning")
		interval = fs.Duration("interval", 2*time.Second, "for tail: how often to look for new events")
	)
	action, actionArgs, err := parseAction(fs, args, "")
	if err != nil {
		return err
	}
	if action == "" {
		return errors.New("no action given: want list, get, count or tail")
	}
	resolveDatabase()
	do, err := l0Action(fs, action, actionArgs, l0.ListOptions{
		Filter: l0.Filter{
			Source:   *source,
			Kind:     connector.Kind(*kind),
			Artifact: *artifact,
		},
		Limit:  *limit,
		Newest: *newest,
	}, *cursor, *interval)
	if err != nil {
		return err
	}

	ctx, err = withLogger(ctx, "l0", cfg, stderr)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return do(ctx, l0.New(pool), stdout)
}

func l0Action(fs *flag.FlagSet, action string, args []string, opts l0.ListOptions, cursor string, interval time.Duration) (func(context.Context, *l0.Store, io.Writer) error, error) {
	switch action {
	case "list":
		if err := checkArgs(fs, action, args, "source", "kind", "artifact", "limit", "newest"); err != nil {
			return nil, err
		}
		if err := opts.Validate(); err != nil {
			return nil, err
		}
		return func(ctx context.Context, store *l0.Store, w io.Writer) error {
			events, err := store.List(ctx, opts)
			if err != nil {
				return err
			}
			printEvents(w, events)
			return nil
		}, nil

	case "get":
		if err := checkFlags(fs, action); err != nil {
			return nil, err
		}
		if len(args) != 1 {
			return nil, errors.New("get takes one argument: the event id")
		}
		id := args[0]
		return func(ctx context.Context, store *l0.Store, w io.Writer) error {
			event, err := store.Get(ctx, id)
			if err != nil {
				return err
			}
			encoded, err := json.MarshalIndent(event, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding event %s: %w", event.ID, err)
			}
			fmt.Fprintf(w, "%s\n", encoded)
			return nil
		}, nil

	case "count":
		if err := checkArgs(fs, action, args); err != nil {
			return nil, err
		}
		return func(ctx context.Context, store *l0.Store, w io.Writer) error {
			counts, err := store.Counts(ctx)
			if err != nil {
				return err
			}
			printCounts(w, counts)
			return nil
		}, nil

	case "tail":
		if err := checkArgs(fs, action, args, "source", "kind", "artifact", "limit", "cursor", "interval"); err != nil {
			return nil, err
		}
		if err := opts.Validate(); err != nil {
			return nil, err
		}
		from, err := l0.ParseCursor(cursor)
		if err != nil {
			return nil, err
		}
		if interval <= 0 {
			return nil, fmt.Errorf("--interval must be positive, and is %s", interval)
		}
		return func(ctx context.Context, store *l0.Store, w io.Writer) error {
			return tail(ctx, w, store, from, opts.Filter, opts.Limit, interval)
		}, nil

	default:
		return nil, fmt.Errorf("unknown action %q: want list, get, count or tail", action)
	}
}

func tail(ctx context.Context, w io.Writer, store *l0.Store, from l0.Cursor, filter l0.Filter, limit int, interval time.Duration) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "CURSOR\tINGESTED AT\tSOURCE\tKIND\tARTIFACT\tID\n")
	_ = tw.Flush()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		changes, err := store.Changes(ctx, from, filter, limit)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, change := range changes {
			from = change.Cursor
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				change.Cursor, change.IngestedAt.UTC().Format(time.RFC3339),
				change.Event.Source, change.Event.Kind, change.Event.Payload.Artifact, change.Event.ID)
		}
		_ = tw.Flush()

		if len(changes) > 0 && len(changes) == l0.Limit(limit) {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func printEvents(w io.Writer, events []connector.Event) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "TIME\tSOURCE\tKIND\tARTIFACT\tID\n")
	for _, ev := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ev.Time.UTC().Format(time.RFC3339), ev.Source, ev.Kind, ev.Payload.Artifact, ev.ID)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "%d event(s)\n", len(events))
}

func printCounts(w io.Writer, counts []l0.Count) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "SOURCE\tKIND\tEVENTS\tVISIBLE\n")
	var events, visible int64
	for _, c := range counts {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", c.Source, c.Kind, c.Events, c.Visible)
		events += c.Events
		visible += c.Visible
	}
	fmt.Fprintf(tw, "total\t\t%d\t%d\n", events, visible)
	_ = tw.Flush()
}

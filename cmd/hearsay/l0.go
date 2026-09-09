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

// runL0 is the L0 inspect subcommand: what is in the event store, for an
// operator asking why a source is quiet or where a bundle's evidence came from.
// It is read-only — nothing here writes an event — and it reads the store
// directly, so `get` prints an event's payload in full. That is what an
// inspection tool is for; ADR-0008's rule is about log lines, which go to
// whoever can read the logs rather than to whoever ran the command.
func runL0(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, _ := newFlagSet("l0", stderr)
	databaseFlag(fs, cfg)
	var (
		source   = fs.String("source", "", "only events from this configured source")
		kind     = fs.String("kind", "", "only events of this kind")
		artifact = fs.String("artifact", "", "only this artifact's history; needs --source")
		limit    = fs.Int("limit", 0, "how many events to return (default 100, maximum 1000)")
		newest   = fs.Bool("newest", false, "for `list`: newest first")
		cursor   = fs.String("cursor", "", "for `tail`: resume after this cursor instead of the beginning")
		interval = fs.Duration("interval", 2*time.Second, "for `tail`: how often to look for new events")
	)
	action, err := parseAction(fs, args, "")
	if err != nil {
		return err
	}
	if action == "" {
		return errors.New("no action given: want list, get, count or tail")
	}
	// Work out what to do before opening anything, so that a typo in the action
	// is a typo rather than a connection failure.
	do, err := l0Action(fs, action, l0.ListOptions{
		Source:   *source,
		Kind:     connector.Kind(*kind),
		Artifact: *artifact,
		Limit:    *limit,
		Newest:   *newest,
	}, *cursor, *interval)
	if err != nil {
		return err
	}

	ctx, err = withLogger(ctx, "l0", cfg, stderr)
	if err != nil {
		return err
	}
	// Connect rather than Open: an inspect command reading a database older
	// than its own migrations would report a schema it cannot see (ADR-0006).
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return do(ctx, l0.New(pool), stdout)
}

// l0Action turns the action word and what follows it into the work to do. It is
// the one place the vocabulary lives.
func l0Action(fs *flag.FlagSet, action string, opts l0.ListOptions, cursor string, interval time.Duration) (func(context.Context, *l0.Store, io.Writer) error, error) {
	switch action {
	case "list":
		if err := noArgs(fs, action); err != nil {
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
		if fs.NArg() != 1 {
			return nil, errors.New("get takes one argument: the event id")
		}
		id := fs.Arg(0)
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
		if err := noArgs(fs, action); err != nil {
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
		if err := noArgs(fs, action); err != nil {
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
			return tail(ctx, w, store, from, opts.Limit, interval)
		}, nil

	default:
		return nil, fmt.Errorf("unknown action %q: want list, get, count or tail", action)
	}
}

// tail follows the change feed until the process is interrupted, which is why
// it returns nil on a cancelled context: Ctrl-C is how this command ends.
//
// It reads the same feed the distiller does, so what it prints is what a
// consumer at that cursor would be handed — tombstoned events included by their
// absence.
func tail(ctx context.Context, w io.Writer, store *l0.Store, from l0.Cursor, limit int, interval time.Duration) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "CURSOR\tINGESTED AT\tSOURCE\tKIND\tARTIFACT\tID\n")
	_ = tw.Flush()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		changes, err := store.Changes(ctx, from, limit)
		if err != nil {
			// A cancelled context surfaces as a failed query; ending on Ctrl-C
			// is not a failure of the command.
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

		// A full page means there is more waiting, so read again rather than
		// sleeping a tick for every page of a backlog.
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

// printEvents prints what an event is and where it came from, and no payload:
// a listing is read at a glance and often over someone's shoulder, and `get` is
// how you ask for the content of one.
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

// printCounts prints both totals: what was ever ingested, and what a read
// returns. They differ by exactly what tombstones cover, which is how an
// append-only store shows that a deletion kept the row.
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

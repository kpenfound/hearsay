package assertworker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// CommandConsumer is the name the worker reads the L0 change feed under for
// GitHub `/hearsay` comment commands. One name for every replica.
const CommandConsumer = "assert-worker:github-commands"

// The GitHub commands.
const (
	CommandRatify = "ratify"
	CommandDemote = "demote"
	CommandPin    = "pin"
	CommandMerge  = "merge"
)

// Replier answers a GitHub `/hearsay` command with one comment on the issue or
// pull request it was written on, and edits that comment and no other. The
// GitHub connector's [github.Replier] is one.
type Replier interface {
	// Reply posts body in answer to command comment command, written on
	// issue in repo at since, and returns the reply's comment id; a reply to
	// that command already there is returned and nothing is posted.
	Reply(ctx context.Context, repo string, issue int, command int64, since time.Time, body string) (int64, error)
	// Revise replaces the text of the reply. A reply somebody deleted is an
	// error wrapping [github.ErrReplyGone].
	Revise(ctx context.Context, repo string, command, reply int64, body string) error
}

// Replies are the repliers commands are answered through, by source id.
type Replies map[string]Replier

// CommandFollower puts the GitHub `/hearsay` commands L0 holds on the assert
// queue: each command comment as written, and the tombstone of each one that
// was deleted, as a `gesture:<event id>` job under the serial key of the scope
// it acts on (ADR-0022). The jobs and the cursor commit together, so a restart
// misses nothing and a repeated enqueue collapses into the job already
// pending.
type CommandFollower struct {
	pool     *pgxpool.Pool
	repo     config.Repo
	interval time.Duration
	batch    int
}

// NewCommandFollower builds the feed reader. A zero interval or batch is the
// default.
func NewCommandFollower(pool *pgxpool.Pool, repo config.Repo, interval time.Duration, batch int) *CommandFollower {
	if interval <= 0 {
		interval = DefaultFollowInterval
	}
	if batch <= 0 {
		batch = DefaultFollowBatch
	}
	return &CommandFollower{pool: pool, repo: repo, interval: interval, batch: l0.Limit(batch)}
}

// Run reads the feed until ctx is cancelled, and returns nil when it stops
// that way. A read that fails is logged and retried at the next tick.
func (f *CommandFollower) Run(ctx context.Context) error {
	return follow(ctx, f.interval, f.batch, f.Once, "github commands")
}

// Once reads one batch of the feed, enqueues the commands in it, and returns
// how many events it read.
//
// An edit of a command comment is not enqueued: Hearsay runs a command as it
// was written. A command deleted before the follower read it is not in the
// feed at all, and its tombstone finds nothing to undo.
func (f *CommandFollower) Once(ctx context.Context) (int, error) {
	from, err := l0.NewCursors(f.pool).Load(ctx, CommandConsumer)
	if err != nil {
		return 0, err
	}
	changes, err := l0.New(f.pool).Changes(ctx, from, l0.Filter{}, f.batch)
	if err != nil {
		return 0, err
	}
	if len(changes) == 0 {
		return 0, nil
	}
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		events, graph := l0.New(tx), l2.New(tx)
		for _, change := range changes {
			ev := change.Event
			if src, ok := f.repo.Source(ev.Source); !ok || src.Type != github.Type {
				continue
			}
			command := ev
			switch ev.Kind {
			case connector.KindCommand:
			case connector.KindTombstone:
				retracted, err := events.Retracted(ctx, ev.Source, ev.Payload.Target)
				if errors.Is(err, l0.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if retracted.Kind != connector.KindCommand {
					continue
				}
				command = retracted
			default:
				continue
			}
			cmd, err := github.CommandOf(command)
			if err != nil {
				return err
			}
			if ev.Kind == connector.KindCommand && cmd.Edited {
				continue
			}
			key, err := commandScope(ctx, graph, f.repo, command, cmd)
			if err != nil {
				return err
			}
			if _, err := queue.Enqueue(ctx, tx, queue.Request{Kind: l2.AssertKind(), TargetID: l2.GestureTarget + ev.ID, SerialKey: key}); err != nil {
				return err
			}
		}
		_, err := l0.NewCursors(tx).Save(ctx, CommandConsumer, changes[len(changes)-1].Cursor)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("following github commands: %w", err)
	}
	return len(changes), nil
}

// commandScope is the serial key a command runs under: the scope of the two
// topics a merge names, where they exist and share one, and otherwise the
// scope of the issue or pull request it was written on, which is the scope of
// every stance drawn from it. A topic never changes scope, so the command and
// the tombstone that undoes it are keyed alike.
func commandScope(ctx context.Context, graph *l2.Store, repo config.Repo, ev connector.Event, cmd github.Command) (string, error) {
	key := l2.ScopeKey(repo, ev.Source, ev.Payload.Container.NativeID)
	if cmd.Name != CommandMerge || len(cmd.Args) != 2 {
		return key, nil
	}
	scopes := make([]string, 2)
	for i, id := range cmd.Args {
		t, err := graph.Topic(ctx, id)
		if errors.Is(err, l2.ErrNotFound) {
			return key, nil
		}
		if err != nil {
			return "", err
		}
		scopes[i] = t.Scope
	}
	if scopes[0] != scopes[1] {
		return key, nil
	}
	return scopes[0], nil
}

// follow is a feed follower's loop: once until a batch comes back short, then
// wait for the next tick. A read that fails is logged as the named follower's
// and retried at the next tick.
func follow(ctx context.Context, interval time.Duration, batch int, once func(context.Context) (int, error), follower string) error {
	log := telemetry.Logger(ctx)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		for ctx.Err() == nil {
			n, err := once(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.ErrorContext(ctx, "following the change feed failed", "follower", follower, "error", err)
				}
				break
			}
			if n < batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// commandRecord is a row of github_command_replies: what a command did and
// the reply that says so.
type commandRecord struct {
	Event, Source, Artifact string
	Repo                    string
	Issue                   int
	Comment                 int64
	CommentedAt             time.Time
	Principal               string
	Gesture, Operation      int64
	Body                    string
	Reply                   int64
	UndoEvent, UndoBody     string
	UndoRevised             bool
}

// text is what the reply says: what the command did, and what its deletion
// did, if it has been deleted.
func (r commandRecord) text() string {
	if r.UndoBody == "" {
		return r.Body
	}
	return r.Body + "\n\n" + r.UndoBody
}

const commandColumns = `event, source, artifact, repository, issue, comment, commented_at, coalesce(principal, ''),
    coalesce(gesture, 0), coalesce(operation, 0), body, coalesce(reply, 0), coalesce(undo_event, ''),
    coalesce(undo_body, ''), undo_revised`

// commandRun is the record of the command a comment ran, and false where it
// ran none.
func commandRun(ctx context.Context, q l2.Querier, source, artifact string, lock bool) (commandRecord, bool, error) {
	sql := `SELECT ` + commandColumns + ` FROM github_command_replies WHERE source = $1 AND artifact = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var r commandRecord
	err := q.QueryRow(ctx, sql, source, artifact).Scan(&r.Event, &r.Source, &r.Artifact, &r.Repo, &r.Issue, &r.Comment,
		&r.CommentedAt, &r.Principal, &r.Gesture, &r.Operation, &r.Body, &r.Reply, &r.UndoEvent, &r.UndoBody, &r.UndoRevised)
	if errors.Is(err, pgx.ErrNoRows) {
		return commandRecord{}, false, nil
	}
	if err != nil {
		return commandRecord{}, false, fmt.Errorf("reading the command comment %s: %w", artifact, err)
	}
	return r, true, nil
}

func nullable[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// errWaiting is a command whose target is still being distilled or asserted:
// the job is retried, and answered from what is there on its last attempt.
var errWaiting = errors.New("waiting for the document the command acts on")

// runCommand runs a `/hearsay` comment and answers it. A comment that has run
// is not run again — a retried job only posts the reply it still owes — and an
// edit of one runs nothing.
func (a *Asserter) runCommand(ctx context.Context, job queue.Job, ev connector.Event) error {
	cmd, err := github.CommandOf(ev)
	if err != nil {
		return err
	}
	if cmd.Edited {
		return nil
	}
	rec, found, err := commandRun(ctx, a.pool, ev.Source, ev.Payload.Artifact, false)
	if err != nil {
		return err
	}
	if found && rec.Event != ev.ID {
		return nil
	}
	if !found {
		err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
			rec = commandRecord{
				Event: ev.ID, Source: ev.Source, Artifact: ev.Payload.Artifact, Repo: cmd.Repo, Issue: cmd.Issue,
				Comment: cmd.Comment, CommentedAt: ev.Time,
			}
			if rec.Body, err = a.apply(ctx, tx, job, ev, cmd, &rec); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
INSERT INTO github_command_replies (event, source, artifact, repository, issue, comment, commented_at, principal, gesture, operation, body)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				rec.Event, rec.Source, rec.Artifact, rec.Repo, rec.Issue, rec.Comment, rec.CommentedAt,
				nullable(rec.Principal), nullable(rec.Gesture), nullable(rec.Operation), rec.Body)
			if err != nil {
				return fmt.Errorf("recording the command %s: %w", ev.ID, err)
			}
			return nil
		})
		if errors.Is(err, errWaiting) {
			telemetry.Logger(ctx).InfoContext(ctx, "github command waits for its document", "l0_id", ev.ID, "attempt", job.Attempt)
			return err
		}
		if err != nil {
			return fmt.Errorf("running the command %s: %w", ev.ID, err)
		}
		telemetry.Logger(ctx).InfoContext(ctx, "github command run", "l0_id", ev.ID, "scope", job.SerialKey,
			"principal", rec.Principal, "gesture", rec.Gesture, "operation", rec.Operation)
	}
	return a.answer(ctx, rec)
}

// apply does what a command asks, in the transaction that records it, and
// returns the reply: the result, or why nothing was done. The L2 write is made
// under a savepoint, so a refusal leaves the transaction whole to record it.
//
// The reply names ids and never quotes a topic, a position or anything else
// read from a document: it is read by everyone who can read the issue, who
// may not be able to read what the commenter can.
func (a *Asserter) apply(ctx context.Context, tx pgx.Tx, job queue.Job, ev connector.Event, cmd github.Command, rec *commandRecord) (string, error) {
	switch cmd.Name {
	case CommandRatify, CommandDemote, CommandPin:
		if len(cmd.Args) > 0 {
			return fmt.Sprintf("`/hearsay %s` takes nothing after it: it acts on the issue or pull request it is written on. Hearsay changed nothing.", cmd.Name), nil
		}
	case CommandMerge:
		if len(cmd.Args) != 2 || cmd.Args[0] == cmd.Args[1] {
			return "`/hearsay merge` takes two different topic ids, the topic to merge and the topic it goes into, as `hearsay topics list` prints them: `/hearsay merge <topic-id> <topic-id>`. Hearsay changed nothing.", nil
		}
	default:
		return "Hearsay has no such command. It takes `/hearsay ratify`, `/hearsay demote`, `/hearsay pin` and `/hearsay merge <topic-id> <topic-id>`. Hearsay changed nothing.", nil
	}
	human, refusal, err := a.commenter(ev)
	if err != nil || refusal != "" {
		return refusal, err
	}
	rec.Principal = human.ID
	view, err := l2.NewView(ctx, l2.New(tx), a.repo, human)
	if err != nil {
		return "", err
	}
	if cmd.Name == CommandMerge {
		return a.merge(ctx, tx, job, view, human, cmd, rec)
	}
	return a.gesture(ctx, tx, job, view, human, ev, cmd, rec)
}

// commenter is the configured human a comment's author maps to, or the reply
// that explains why there is none.
func (a *Asserter) commenter(ev connector.Event) (principal.Principal, string, error) {
	const unmapped = "Your GitHub account is not mapped to a Hearsay principal, so Hearsay changed nothing. Ask whoever configures Hearsay to add it to your principal's `identities`."
	if ev.Payload.Author == nil {
		return principal.Principal{}, unmapped, nil
	}
	resolver, err := a.repo.Resolver()
	if err != nil {
		return principal.Principal{}, "", err
	}
	res := resolver.Resolve(*ev.Payload.Author)
	switch res.Status {
	case principal.Resolved:
	case principal.Ambiguous:
		return principal.Principal{}, fmt.Sprintf("Your GitHub account maps to more than one Hearsay principal (%s), so Hearsay changed nothing. Ask whoever configures Hearsay to fix the mapping.", strings.Join(res.Candidates, ", ")), nil
	default:
		return principal.Principal{}, unmapped, nil
	}
	if res.Principal.Kind != principal.KindHuman {
		return principal.Principal{}, fmt.Sprintf("Your GitHub account maps to `%s`, which is not a person. Only a person can ratify, demote, pin or merge, so Hearsay changed nothing.", res.Principal.ID), nil
	}
	return res.Principal, "", nil
}

// gesture ratifies, demotes or pins the document of the issue or pull request
// the command was written on.
func (a *Asserter) gesture(ctx context.Context, tx pgx.Tx, job queue.Job, view *l2.View, human principal.Principal, ev connector.Event, cmd github.Command, rec *commandRecord) (string, error) {
	doc := l1.DocID(ev.Source, ev.Payload.Parent)
	notYet := "Hearsay has not distilled this conversation into anything you can read yet, so it changed nothing. Try again once it has."
	got, err := l1.New(tx).Get(ctx, doc)
	switch {
	case errors.Is(err, l1.ErrNotFound):
		if err := a.wait(ctx, tx, job, doc); err != nil {
			return "", err
		}
		return notYet, nil
	case err != nil:
		return "", err
	case !view.Reader().MayRead(got.Document):
		return notYet, nil
	}
	noun := "issue"
	if got.Kind == l1.KindPR {
		noun = "pull request"
	}
	req := l2.GestureRequest{Event: ev.ID, Principal: human.ID, Action: l2.GestureAction(cmd.Name), Documents: []string{doc}}
	var g l2.Gesture
	err = savepoint(ctx, tx, func(tx pgx.Tx) error {
		g, _, err = l2.New(tx).ApplyGesture(ctx, a.repo, job.SerialKey, req)
		return err
	})
	if errors.Is(err, l2.ErrNotFound) && cmd.Name != CommandPin {
		// No live stance is drawn from it: perhaps not yet.
		if err := a.wait(ctx, tx, job, doc); err != nil {
			return "", err
		}
		return fmt.Sprintf("Hearsay holds no stance drawn from this %s, so there is nothing to %s and it changed nothing.", noun, cmd.Name), nil
	}
	if refusal, err := refusal(cmd.Name+" this "+noun, err); err != nil || refusal != "" {
		return refusal, err
	}
	rec.Gesture = g.ID
	undo := fmt.Sprintf("To undo it, delete your `/hearsay %s` comment, or run `hearsay gestures undo %d`.", cmd.Name, g.ID)
	switch cmd.Name {
	case CommandRatify:
		return fmt.Sprintf("Ratified %s drawn from this %s in scope `%s`, as gesture %d. %s", stances(len(g.Stances)), noun, g.Scope, g.ID, undo), nil
	case CommandDemote:
		return fmt.Sprintf("Demoted %s drawn from this %s in scope `%s`, as gesture %d: Hearsay serves them as contested until a person ratifies them or a newer stance supersedes them. %s", stances(len(g.Stances)), noun, g.Scope, g.ID, undo), nil
	}
	return fmt.Sprintf("Pinned this %s as an anchor in scope `%s`, as gesture %d. %s", noun, g.Scope, g.ID, undo), nil
}

func stances(n int) string {
	if n == 1 {
		return "1 stance"
	}
	return fmt.Sprintf("%d stances", n)
}

// merge merges the first topic the command names into the second, when the
// commenter may read both.
func (a *Asserter) merge(ctx context.Context, tx pgx.Tx, job queue.Job, view *l2.View, human principal.Principal, cmd github.Command, rec *commandRecord) (string, error) {
	from, into := cmd.Args[0], cmd.Args[1]
	for _, id := range []string{from, into} {
		t, err := view.Topic(ctx, id)
		if err != nil {
			return "", err
		}
		if t == nil {
			return fmt.Sprintf("Hearsay has no topic `%s` that you can read, so it merged nothing. `hearsay topics list <scope>` prints the ids of the topics you can read.", id), nil
		}
	}
	var op l2.Operation
	err := savepoint(ctx, tx, func(tx pgx.Tx) error {
		var err error
		op, err = l2.New(tx).ApplyOperation(ctx, a.repo, job.SerialKey, l2.OperationRequest{Kind: l2.OperationMerge, Principal: human.ID, From: from, Into: into})
		return err
	})
	if refusal, err := refusal("merge these topics", err); err != nil || refusal != "" {
		return refusal, err
	}
	rec.Operation = op.ID
	return fmt.Sprintf("Merged topic `%s` into topic `%s` in scope `%s`, as operation %d. To undo it, delete your `/hearsay merge` comment, or run `hearsay topics undo %d`.", from, into, op.Scope, op.ID, op.ID), nil
}

// wait returns errWaiting while the document a command acts on is still being
// distilled or asserted, unless this is the job's last attempt.
func (a *Asserter) wait(ctx context.Context, tx pgx.Tx, job queue.Job, doc string) error {
	if job.Attempt >= queue.DefaultMaxAttempts {
		return nil
	}
	for _, kind := range []queue.Kind{distiller.JobKind(), l2.AssertKind()} {
		pending, err := queue.Unfinished(ctx, tx, kind, []string{doc})
		if err != nil {
			return err
		}
		if pending[doc] {
			return fmt.Errorf("%w: %s has an unfinished %s job", errWaiting, doc, kind.Name)
		}
	}
	return nil
}

// savepoint runs fn in a savepoint of tx, and rolls it back if fn fails, so
// that the failure is one tx can still record.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting a savepoint: %w", err)
	}
	if err := fn(sp); err != nil {
		if rollback := sp.Rollback(ctx); rollback != nil {
			return errors.Join(err, rollback)
		}
		return err
	}
	return sp.Commit(ctx)
}

// refusal explains an error from a ledger as a reply: a refusal says why, and
// anything else is returned to be retried.
func refusal(what string, err error) (string, error) {
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, l2.ErrNotAllowed):
		return fmt.Sprintf("You may not %s: %v. Hearsay changed nothing.", what, err), nil
	case errors.Is(err, l2.ErrInvalid), errors.Is(err, l2.ErrNotFound), errors.Is(err, l2.ErrConflict):
		return fmt.Sprintf("Hearsay could not %s: %v. It changed nothing.", what, err), nil
	}
	return "", err
}

// undoCommand undoes what a deleted command comment did, and says so in its
// reply. A command that was refused, or never ran, has nothing to undo.
func (a *Asserter) undoCommand(ctx context.Context, job queue.Job, tomb connector.Event) error {
	rec, found, err := commandRun(ctx, a.pool, tomb.Source, tomb.Payload.Target, false)
	if err != nil || !found || (rec.Gesture == 0 && rec.Operation == 0) {
		return err
	}
	if rec.UndoEvent == "" {
		err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
			if rec, _, err = commandRun(ctx, tx, tomb.Source, tomb.Payload.Target, true); err != nil || rec.UndoEvent != "" {
				return err
			}
			rec.UndoEvent = tomb.ID
			if rec.UndoBody, err = a.undo(ctx, tx, job, tomb, rec); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE github_command_replies SET undo_event = $2, undo_body = $3 WHERE event = $1`, rec.Event, rec.UndoEvent, rec.UndoBody)
			if err != nil {
				return fmt.Errorf("recording the undo of %s: %w", rec.Event, err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("undoing the command %s: %w", rec.Event, err)
		}
		telemetry.Logger(ctx).InfoContext(ctx, "github command undone", "l0_id", rec.Event, "tombstone", tomb.ID, "scope", job.SerialKey)
	}
	return a.answer(ctx, rec)
}

// undo reverses a command's gesture or operation and returns what its reply
// now says about it.
func (a *Asserter) undo(ctx context.Context, tx pgx.Tx, job queue.Job, tomb connector.Event, rec commandRecord) (string, error) {
	const deleted = "**Update:** the command comment was deleted, "
	if rec.Gesture != 0 {
		var g l2.Gesture
		err := savepoint(ctx, tx, func(tx pgx.Tx) error {
			var err error
			g, _, err = l2.New(tx).ApplyGesture(ctx, a.repo, job.SerialKey, l2.GestureRequest{Event: tomb.ID, Principal: rec.Principal, Action: l2.GestureUndo, Undoes: rec.Event})
			return err
		})
		if refusal, err := refusal(fmt.Sprintf("undo gesture %d", rec.Gesture), err); err != nil || refusal != "" {
			return deleted + "but it was not undone. " + refusal, err
		}
		return fmt.Sprintf(deleted+"so Hearsay undid gesture %d, as gesture %d.", rec.Gesture, g.ID), nil
	}
	var op l2.Operation
	err := savepoint(ctx, tx, func(tx pgx.Tx) error {
		var err error
		op, err = l2.New(tx).ApplyOperation(ctx, a.repo, job.SerialKey, l2.OperationRequest{Kind: l2.OperationUndo, Principal: rec.Principal, Undoes: rec.Operation})
		return err
	})
	if conflict := (*l2.ConflictError)(nil); errors.As(err, &conflict) {
		ids := make([]string, len(conflict.Conflicting))
		for i, id := range conflict.Conflicting {
			ids[i] = fmt.Sprint(id)
		}
		if slices.Contains(conflict.Conflicting, conflict.Operation) || strings.HasPrefix(conflict.Reason, "was already undone") {
			return fmt.Sprintf(deleted+"but operation %d had already been undone, by operation %s, so there was nothing left to undo.", rec.Operation, strings.Join(ids, ", ")), nil
		}
		return fmt.Sprintf(deleted+"but Hearsay could not undo operation %d, because later operations on its topics are still in force: operation %s. Undo those first with `hearsay topics undo`, then run `hearsay topics undo %d`.",
			rec.Operation, strings.Join(ids, ", "), rec.Operation), nil
	}
	if refusal, err := refusal(fmt.Sprintf("undo operation %d", rec.Operation), err); err != nil || refusal != "" {
		return deleted + "but it was not undone. " + refusal, err
	}
	return fmt.Sprintf(deleted+"so Hearsay undid operation %d, as operation %d.", rec.Operation, op.ID), nil
}

// answer posts the reply a command owes, or finds the one already posted, and
// revises it once the command's deletion has been applied. What it did is
// recorded, so a retry makes no write it has made.
func (a *Asserter) answer(ctx context.Context, rec commandRecord) error {
	log := telemetry.Logger(ctx)
	replier := a.replies[rec.Source]
	if replier == nil {
		log.WarnContext(ctx, "no replier for the source: the command is not answered", "source", rec.Source, "l0_id", rec.Event)
		return nil
	}
	if rec.Reply == 0 {
		id, err := replier.Reply(ctx, rec.Repo, rec.Issue, rec.Comment, rec.CommentedAt, rec.text())
		if err != nil {
			return err
		}
		if _, err := a.pool.Exec(ctx, `UPDATE github_command_replies SET reply = $2 WHERE event = $1`, rec.Event, id); err != nil {
			return fmt.Errorf("recording the reply to %s: %w", rec.Event, err)
		}
		rec.Reply = id
		log.InfoContext(ctx, "github command answered", "l0_id", rec.Event, "reply", id)
	}
	if rec.UndoEvent == "" || rec.UndoRevised {
		return nil
	}
	err := replier.Revise(ctx, rec.Repo, rec.Comment, rec.Reply, rec.text())
	if errors.Is(err, github.ErrReplyGone) {
		log.InfoContext(ctx, "the reply to a deleted github command is gone: not revised", "l0_id", rec.Event, "reply", rec.Reply)
	} else if err != nil {
		return err
	}
	if _, err := a.pool.Exec(ctx, `UPDATE github_command_replies SET undo_revised = true WHERE event = $1`, rec.Event); err != nil {
		return fmt.Errorf("recording the revised reply to %s: %w", rec.Event, err)
	}
	return nil
}

// githubGesture is the assert job for a GitHub command event or the tombstone
// of one: `gesture:<event id>`.
func (a *Asserter) githubGesture(ctx context.Context, job queue.Job) error {
	id := strings.TrimPrefix(job.TargetID, l2.GestureTarget)
	ev, err := a.events.Get(ctx, id)
	if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
		// A command deleted before it ran runs nothing; its tombstone's job
		// finds nothing to undo.
		return nil
	}
	if err != nil {
		return err
	}
	if src, ok := a.repo.Source(ev.Source); !ok || src.Type != github.Type {
		return nil
	}
	switch ev.Kind {
	case connector.KindCommand:
		return a.runCommand(ctx, job, ev)
	case connector.KindTombstone:
		return a.undoCommand(ctx, job, ev)
	}
	return nil
}

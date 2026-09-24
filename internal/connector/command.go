package connector

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// The chat commands a person can give Hearsay in a source: the verbs of a
// [Command].
const (
	// CommandPin pins the document of the artifact the command names, as an
	// anchor in its scope.
	CommandPin = "pin"
	// CommandMerge merges one topic into another.
	CommandMerge = "merge"
)

var (
	// ErrReadOnly is what [CommandSink.Command] returns for a source configured
	// `read_only: true`. Nothing was recorded or applied, and the connector
	// answers nothing: Hearsay takes no commands in such a source.
	ErrReadOnly = errors.New("the source is read-only and takes no commands")
	// ErrNoCommandApplier is what [CommandSink.Command] returns from a runtime
	// built with nothing to apply commands through. Nothing was recorded.
	ErrNoCommandApplier = errors.New("the runtime has no command applier")
)

// Command is a chat slash command a connector received and parsed: the L0
// event it is recorded as, and what it asks for. The connector parses the
// source's own request into one; recording it, applying it and deciding the
// result are the runtime's.
type Command struct {
	// Event is the `command` event the command is recorded as, under the
	// source it was given in, with an id the source gives that command once,
	// so that a request delivered twice is one event. Its author is who ran
	// the command, and is whom the command is applied as.
	Event Event
	// Verb is [CommandPin] or [CommandMerge]. Anything else is answered as a
	// command Hearsay does not have.
	Verb string
	// Target is, for a pin, the artifact whose L1 document is pinned — the
	// thread the command was run in — and empty where there is none.
	Target string
	// From and Into are, for a merge, the topic ids it names.
	From, Into string
}

// CommandRequest is a recorded command as the applier is handed it.
type CommandRequest struct {
	// Source is the source the command was given in.
	Source string
	// Event is the id of the recorded L0 `command` event. A pin's gesture is
	// keyed by it, so applying the same command twice records one gesture.
	Event string
	// Invoker is who ran it, as a source identity.
	Invoker Identity
	// Verb, Target, From and Into are the [Command]'s.
	Verb, Target, From, Into string
}

// CommandApplier applies a recorded command as the configured human its
// invoker maps to, and reports what it did or why it did nothing. It is
// `internal/l2`'s, which a process that receives commands wires into its
// runtime; the connector package only names it.
type CommandApplier interface {
	Apply(ctx context.Context, req CommandRequest) CommandResult
}

// CommandOutcome is what became of a command: applied, or refused for a
// reason the person can be told.
type CommandOutcome string

// The outcomes. The first three applied the command; every other one changed
// nothing.
const (
	// CommandPinned is a pin recorded as gesture [CommandResult.Gesture].
	CommandPinned CommandOutcome = "pinned"
	// CommandAlreadyPinned is a pin this same command event had already
	// recorded, as gesture [CommandResult.Gesture].
	CommandAlreadyPinned CommandOutcome = "already_pinned"
	// CommandMerged is a merge recorded as operation
	// [CommandResult.Operation].
	CommandMerged CommandOutcome = "merged"

	// CommandNotRead is a command in a container the source's allowlist does
	// not admit. It was not recorded.
	CommandNotRead CommandOutcome = "not_read"
	// CommandNotRecorded is a command whose event could not be written to L0,
	// so it was not applied. Running it again may work.
	CommandNotRecorded CommandOutcome = "not_recorded"
	// CommandDeleted is a command whose event an operator deleted from
	// Hearsay (ADR-0018), delivered again.
	CommandDeleted CommandOutcome = "deleted"
	// CommandUnknown is a verb Hearsay does not have.
	CommandUnknown CommandOutcome = "unknown"
	// CommandUnmapped is an invoker no configured principal claims.
	CommandUnmapped CommandOutcome = "unmapped"
	// CommandAmbiguous is an invoker more than one principal claims,
	// [CommandResult.Candidates].
	CommandAmbiguous CommandOutcome = "ambiguous"
	// CommandNotHuman is an invoker who maps to [CommandResult.Principal], of
	// kind [CommandResult.Kind], which is not a person.
	CommandNotHuman CommandOutcome = "not_human"
	// CommandNoTarget is a pin that names no artifact: it was not run inside
	// a thread.
	CommandNoTarget CommandOutcome = "no_target"
	// CommandUndistilled is a pin of an artifact with no L1 document the
	// person may read, whether it is not distilled yet or hidden from them.
	CommandUndistilled CommandOutcome = "undistilled"
	// CommandSameTopic is a merge of a topic into itself.
	CommandSameTopic CommandOutcome = "same_topic"
	// CommandNoSuchTopic is a merge naming [CommandResult.Topic], which the
	// person cannot read or which does not exist; the two are not told apart.
	CommandNoSuchTopic CommandOutcome = "no_such_topic"
	// CommandNotAllowed is the ledger refusing the person
	// ([CommandResult.Reason]): the scope's `ratified_by.principals` does not
	// name them.
	CommandNotAllowed CommandOutcome = "not_allowed"
	// CommandRejected is the ledger refusing the request itself
	// ([CommandResult.Reason]): invalid, conflicting or naming nothing.
	CommandRejected CommandOutcome = "rejected"
	// CommandViewFailed is a failure to read the graph as the person.
	CommandViewFailed CommandOutcome = "view_failed"
	// CommandReadFailed is a failure to read what the command names: the
	// pinned artifact's document, or a merge's topics.
	CommandReadFailed CommandOutcome = "read_failed"
	// CommandFailed is an internal failure of the ledger write.
	CommandFailed CommandOutcome = "failed"
)

// CommandResult is what a command did, as a value the connector turns into
// its own ephemeral answer. It carries ids and names the person may read, and
// nothing that depends on how the source answers.
type CommandResult struct {
	Outcome CommandOutcome
	// Principal is the principal the invoker maps to, and Kind its kind, once
	// the invoker is mapped.
	Principal string
	Kind      string
	// Candidates are the principals an ambiguous invoker maps to.
	Candidates []string
	// Scope is the scope key a pin or merge was recorded under.
	Scope string
	// Gesture is the gesture a pin recorded; Operation the operation a merge
	// recorded.
	Gesture   int64
	Operation int64
	// Topic is the topic a [CommandNoSuchTopic] names.
	Topic string
	// FromName and IntoName are the names of a merge's topics.
	FromName, IntoName string
	// Reason is the ledger's own account of a [CommandNotAllowed] or a
	// [CommandRejected].
	Reason string
}

// Applied reports whether the command changed anything, now or when this
// same command was first applied.
func (r CommandResult) Applied() bool {
	return r.Outcome == CommandPinned || r.Outcome == CommandAlreadyPinned || r.Outcome == CommandMerged
}

// CommandSink is the runtime's command capability. A connector that receives
// chat commands type-asserts its sink to it and hands it each command it
// parses; the result, or [ErrReadOnly], tells it what to answer. It is safe
// for concurrent use.
//
// The runtime records the command's event as L0, never distilled, then
// applies it through the process's [CommandApplier] and returns the result
// for the connector to deliver ephemerally. The applier writes under the
// scope's serial key; a process that dies mid-command leaves it unapplied,
// and nothing retries it (ADR-0025).
type CommandSink interface {
	Command(ctx context.Context, cmd Command) (CommandResult, error)
}

// CommandRecorder is a sink that says whether the command event it recorded
// is one an operator deleted. The L0 store is one; a sink that is not
// records through Emit.
type CommandRecorder interface {
	RecordCommand(ctx context.Context, ev Event) (deleted bool, err error)
}

// Command implements [CommandSink]. A read-only source gets [ErrReadOnly]
// with nothing recorded; an event that is not a valid `command` of this
// source, or of a kind the connector did not declare, is an error, as it is
// from Emit. A command in a container the allowlist does not admit is
// answered [CommandNotRead] and not recorded.
func (g *Gate) Command(ctx context.Context, cmd Command) (CommandResult, error) {
	if g.readOnly {
		return CommandResult{}, ErrReadOnly
	}
	if g.commands == nil {
		return CommandResult{}, ErrNoCommandApplier
	}
	ev := cmd.Event
	if ev.Kind != KindCommand {
		return CommandResult{}, fmt.Errorf("a command is recorded as a %q event, not %q", KindCommand, ev.Kind)
	}
	if ev.Source != g.source {
		return CommandResult{}, fmt.Errorf("%w: source %q, connector is configured for %q", ErrForeignSource, ev.Source, g.source)
	}
	if err := ev.Validate(); err != nil {
		return CommandResult{}, err
	}
	if !g.kinds[ev.Kind] {
		return CommandResult{}, fmt.Errorf("%w: %q", ErrUndeclaredKind, ev.Kind)
	}
	if !g.allow.Allows(ev.Source, ev.Payload.Container.NativeID) {
		return CommandResult{Outcome: CommandNotRead}, nil
	}
	ev.ID = EventID(ev.Source, ev.NativeID)
	log := telemetry.Logger(ctx)
	var deleted bool
	var err error
	if recorder, ok := g.sink.(CommandRecorder); ok {
		deleted, err = recorder.RecordCommand(ctx, ev)
	} else {
		err = g.sink.Emit(ctx, ev)
	}
	switch {
	case err != nil:
		log.ErrorContext(ctx, "recording a command", "source", ev.Source, "event", ev.ID, "error", err)
		return CommandResult{Outcome: CommandNotRecorded}, nil
	case deleted:
		return CommandResult{Outcome: CommandDeleted}, nil
	}
	return g.commands.Apply(ctx, CommandRequest{
		Source: ev.Source, Event: ev.ID, Invoker: *ev.Payload.Author,
		Verb: cmd.Verb, Target: cmd.Target, From: cmd.From, Into: cmd.Into,
	}), nil
}

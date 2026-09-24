package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// DiscordPath is where the API answers a Discord source's interactions: the
// Interactions Endpoint URL its application is given.
func DiscordPath(source string) string { return "/discord/" + source + "/interactions" }

// DefaultAnswerWithin is how long a command's work runs before its answer is
// deferred. Discord drops an interaction not answered within three seconds.
const DefaultAnswerWithin = 2 * time.Second

// followUpWithin bounds a deferred command's work, well inside the fifteen
// minutes an interaction token lasts.
const followUpWithin = 5 * time.Minute

// Interactions is the HTTP interaction adapter for Discord's `/hearsay pin`
// and `/hearsay merge` (ADR-0022). It verifies every request with the
// application's public key and answers every command and autocomplete within
// Discord's deadline. A command is recorded in L0 as a `command` event of its
// source, in a channel that source ingests, and then applied as the person
// the Discord user maps to: a pin through the gesture ledger
// ([l2.RecordGesture], keyed by the command event), a merge through the topic
// ledger ([l2.Operate]). Both hold the scope's serial key and refuse a person
// its `ratified_by.principals` does not name. The answer is ephemeral and
// states the result or the refusal; when the work outlasts the deadline, the
// answer is deferred and edited in once it is done, with the interaction's
// token. Nothing else is written to Discord.
//
// Applying the command is [l2.Commands], the applier every process that
// receives a chat command shares (ADR-0025); this adapter records the command,
// and turns what the applier returns into the answer's words.
//
// Autocomplete offers the topics the person may read, through the same view
// as `hearsay topics list` ([l2.View]), across every scope key the
// configuration can produce, and a merge refuses a topic they may not read as
// one that does not exist.
type Interactions struct {
	db           *pgxpool.Pool
	commands     *l2.Commands
	allow        connector.Allowlist
	apps         map[string]*discord.App
	answerWithin time.Duration
	stopping     context.Context
	stop         context.CancelFunc
	work         sync.WaitGroup
}

// NewInteractions builds the adapter for these applications.
func NewInteractions(pool *pgxpool.Pool, repo config.Repo, apps ...*discord.App) (*Interactions, error) {
	commands, err := l2.NewCommands(pool, repo)
	if err != nil {
		return nil, err
	}
	stopping, stop := context.WithCancel(context.Background())
	h := &Interactions{db: pool, commands: commands, allow: repo.Allowlist(), apps: map[string]*discord.App{},
		answerWithin: DefaultAnswerWithin, stopping: stopping, stop: stop}
	for _, app := range apps {
		h.apps[app.Source] = app
	}
	return h, nil
}

// Register registers each application's commands in its guild. A failure is
// logged and does not stop the API: the commands a previous start registered
// stay in place.
func (h *Interactions) Register(ctx context.Context) {
	for _, app := range h.apps {
		reg, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := app.Register(reg)
		cancel()
		log := telemetry.Logger(ctx)
		if err != nil {
			log.ErrorContext(ctx, "registering discord commands", "source", app.Source, "guild", app.Guild, "error", err)
			continue
		}
		log.InfoContext(ctx, "discord commands registered", "source", app.Source, "guild", app.Guild)
	}
}

// Stop cancels the work of deferred commands and waits for it to end.
func (h *Interactions) Stop() {
	h.stop()
	h.work.Wait()
}

// ServeHTTP answers one interaction on [DiscordPath].
func (h *Interactions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app, ok := h.apps[r.PathValue("source")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequest))
	if err != nil {
		http.Error(w, "the request body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !app.Verify(r.Header, body) {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}
	in, err := discord.ParseInteraction(body)
	if err != nil {
		http.Error(w, "not a discord interaction", http.StatusBadRequest)
		return
	}
	switch in.Type {
	case discord.InteractionPing:
		respond(w, discord.Pong())
	case discord.InteractionAutocomplete:
		ctx, cancel := context.WithTimeout(r.Context(), h.answerWithin)
		defer cancel()
		respond(w, discord.Choices(h.choices(ctx, app, in)))
	case discord.InteractionCommand:
		h.command(w, r, app, in)
	default:
		respond(w, discord.Answer("Hearsay does not handle this kind of interaction."))
	}
}

func respond(w http.ResponseWriter, resp discord.Response) {
	body, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// command runs a command and answers it: at once if the work is done within
// the deadline, and otherwise with a deferral, then an edit when it is done.
func (h *Interactions) command(w http.ResponseWriter, r *http.Request, app *discord.App, in discord.Interaction) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), followUpWithin)
	unhook := context.AfterFunc(h.stopping, cancel)
	var mu sync.Mutex
	deferred := false
	answer := make(chan string, 1)
	sent := make(chan struct{})
	h.work.Add(1)
	go func() {
		defer h.work.Done()
		defer cancel()
		defer unhook()
		text := h.run(ctx, app, in)
		mu.Lock()
		if !deferred {
			answer <- text
			mu.Unlock()
			return
		}
		mu.Unlock()
		select {
		case <-sent:
		case <-ctx.Done():
			return
		}
		if err := app.Edit(ctx, in.Token, text); err != nil {
			telemetry.Logger(ctx).ErrorContext(ctx, "answering a deferred discord command", "source", app.Source, "interaction", in.ID, "error", err)
		}
	}()
	timer := time.NewTimer(h.answerWithin)
	defer timer.Stop()
	select {
	case text := <-answer:
		respond(w, discord.Answer(text))
		return
	case <-timer.C:
	}
	mu.Lock()
	select {
	case text := <-answer:
		mu.Unlock()
		respond(w, discord.Answer(text))
		return
	default:
	}
	deferred = true
	mu.Unlock()
	respond(w, discord.Defer())
	_ = http.NewResponseController(w).Flush()
	close(sent)
}

// run does what a command asks and returns the answer: the result, or why
// nothing was done.
func (h *Interactions) run(ctx context.Context, app *discord.App, in discord.Interaction) string {
	switch {
	case in.Guild != app.Guild:
		return "Hearsay takes commands only in the server it is configured for."
	case in.Command != discord.CommandName || (in.Subcommand != discord.CommandPin && in.Subcommand != discord.CommandMerge):
		return "Hearsay has no such command. It has `/hearsay pin` and `/hearsay merge`."
	case in.User.ID == "":
		return "Hearsay could not tell who ran this command, so it did nothing."
	}
	if container := in.Channel.Container(); container == "" || !h.allow.Allows(app.Source, container) {
		return answerText(connector.CommandResult{Outcome: connector.CommandNotRead}, in.Subcommand)
	}
	ev := app.CommandEvent(in)
	deleted, err := l0.New(h.db).RecordCommand(ctx, ev)
	switch {
	case err != nil:
		telemetry.Logger(ctx).ErrorContext(ctx, "recording a discord command", "source", app.Source, "interaction", in.ID, "error", err)
		return answerText(connector.CommandResult{Outcome: connector.CommandNotRecorded}, in.Subcommand)
	case deleted:
		return answerText(connector.CommandResult{Outcome: connector.CommandDeleted}, in.Subcommand)
	}
	req := connector.CommandRequest{Source: app.Source, Event: connector.EventID(ev.Source, ev.NativeID), Invoker: app.Author(in),
		Verb: connector.CommandMerge, From: in.Options[discord.OptionFrom], Into: in.Options[discord.OptionInto]}
	if in.Subcommand == discord.CommandPin {
		req.Verb = connector.CommandPin
		if in.Channel.IsThread() {
			req.Target = discord.ThreadArtifact(in.Channel.ID)
		}
	}
	return answerText(h.commands.Apply(ctx, req), in.Subcommand)
}

// answerText is the ephemeral answer to a command that came to this result.
func answerText(res connector.CommandResult, verb string) string {
	switch res.Outcome {
	case connector.CommandPinned:
		return fmt.Sprintf("Pinned this thread in scope %s, as gesture %d. Undo it with `hearsay gestures undo %d`.", res.Scope, res.Gesture, res.Gesture)
	case connector.CommandAlreadyPinned:
		return fmt.Sprintf("This command already pinned the thread in scope %s, as gesture %d.", res.Scope, res.Gesture)
	case connector.CommandMerged:
		return fmt.Sprintf("Merged %q into %q in scope %s, as operation %d. Undo it with `hearsay topics undo %d`.", res.FromName, res.IntoName, res.Scope, res.Operation, res.Operation)
	case connector.CommandNotRead:
		return "Hearsay does not read this channel, so it takes no commands here. Run the command in a channel Hearsay reads."
	case connector.CommandNotRecorded:
		return "Hearsay could not record this command, so it did nothing. Try again."
	case connector.CommandDeleted:
		return "This command was deleted from Hearsay, so it did nothing."
	case connector.CommandUnknown:
		return "Hearsay has no such command. It has `/hearsay pin` and `/hearsay merge`."
	case connector.CommandAmbiguous:
		return fmt.Sprintf("Your Discord account maps to more than one Hearsay principal (%s), so Hearsay did nothing. Ask whoever configures Hearsay to fix the mapping.", strings.Join(res.Candidates, ", "))
	case connector.CommandUnmapped:
		return "Your Discord account is not mapped to a Hearsay principal, so Hearsay did nothing. Ask whoever configures Hearsay to add it to your principal's identities."
	case connector.CommandNotHuman:
		return fmt.Sprintf("Your Discord account maps to %q, which is a %s. Only a person can pin or merge.", res.Principal, res.Kind)
	case connector.CommandViewFailed:
		return "Hearsay could not read the graph as you, so it did nothing."
	case connector.CommandNoTarget:
		return "Run `/hearsay pin` inside a thread. It pins the thread's distilled document, and this channel is not a thread."
	case connector.CommandUndistilled:
		return "Hearsay has not distilled this thread yet, so there is nothing to pin. Try again once it has."
	case connector.CommandSameTopic:
		return "Pick two different topics: a topic cannot be merged into itself."
	case connector.CommandNoSuchTopic:
		return fmt.Sprintf("Hearsay has no topic %q that you can read, so it merged nothing. Pick both topics from the list Discord offers.", res.Topic)
	}
	what := "merge these topics"
	if verb == discord.CommandPin {
		what = "pin this thread"
	}
	switch res.Outcome {
	case connector.CommandReadFailed:
		if verb == discord.CommandPin {
			return "Hearsay could not read this thread, so it pinned nothing. Try again."
		}
		return "Hearsay could not read the topics, so it merged nothing. Try again."
	case connector.CommandNotAllowed:
		return fmt.Sprintf("You may not %s: %s.", what, res.Reason)
	case connector.CommandRejected:
		return fmt.Sprintf("Hearsay could not %s: %s.", what, res.Reason)
	}
	return fmt.Sprintf("Hearsay could not %s because of an internal error, and changed nothing. Try again.", what)
}

// choices are the topics a `/hearsay merge` argument may take: those the
// person may read whose name or id holds what they have typed
// ([l2.Commands.MergeChoices]). For `into`, once `from` names a topic they
// may read, only the other topics in its scope. Anyone Hearsay cannot map to
// a person is offered nothing, and so is a request out of time.
func (h *Interactions) choices(ctx context.Context, app *discord.App, in discord.Interaction) []discord.Choice {
	if in.Guild != app.Guild || in.Command != discord.CommandName || in.Subcommand != discord.CommandMerge ||
		(in.Focused != discord.OptionFrom && in.Focused != discord.OptionInto) {
		return nil
	}
	req := l2.ChoiceRequest{Invoker: app.Author(in), Typed: in.Options[in.Focused], Limit: discord.MaxChoices}
	if in.Focused == discord.OptionInto {
		req.From = in.Options[discord.OptionFrom]
	}
	var out []discord.Choice
	for _, t := range h.commands.MergeChoices(ctx, req) {
		out = append(out, discord.Choice{Name: t.Name + " · " + t.Scope, Value: t.ID})
	}
	return out
}

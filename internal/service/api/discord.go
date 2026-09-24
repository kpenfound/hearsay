package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
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
// Autocomplete offers the topics the person may read, through the same view
// as `hearsay topics list` ([l2.View]), across every scope key the
// configuration can produce, and a merge refuses a topic they may not read as
// one that does not exist.
type Interactions struct {
	db           *pgxpool.Pool
	repo         config.Repo
	resolver     *principal.Resolver
	allow        connector.Allowlist
	apps         map[string]*discord.App
	answerWithin time.Duration
	stopping     context.Context
	stop         context.CancelFunc
	work         sync.WaitGroup
}

// NewInteractions builds the adapter for these applications.
func NewInteractions(pool *pgxpool.Pool, repo config.Repo, apps ...*discord.App) (*Interactions, error) {
	resolver, err := repo.Resolver()
	if err != nil {
		return nil, fmt.Errorf("building the identity resolver: %w", err)
	}
	stopping, stop := context.WithCancel(context.Background())
	h := &Interactions{db: pool, repo: repo, resolver: resolver, allow: repo.Allowlist(), apps: map[string]*discord.App{},
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
	log := telemetry.Logger(ctx)
	switch {
	case in.Guild != app.Guild:
		return "Hearsay takes commands only in the server it is configured for."
	case in.Command != discord.CommandName || (in.Subcommand != discord.CommandPin && in.Subcommand != discord.CommandMerge):
		return "Hearsay has no such command. It has `/hearsay pin` and `/hearsay merge`."
	case in.User.ID == "":
		return "Hearsay could not tell who ran this command, so it did nothing."
	}
	if container := in.Channel.Container(); container == "" || !h.allow.Allows(app.Source, container) {
		return "Hearsay does not read this channel, so it takes no commands here. Run the command in a channel Hearsay reads."
	}
	ev := app.CommandEvent(in)
	appended, err := l0.New(h.db).Append(ctx, ev)
	switch {
	case errors.Is(err, l0.ErrRewrite):
	case err != nil:
		log.ErrorContext(ctx, "recording a discord command", "source", app.Source, "interaction", in.ID, "error", err)
		return "Hearsay could not record this command, so it did nothing. Try again."
	case appended.Dropped != "":
		return "This command was deleted from Hearsay, so it did nothing."
	}
	event := connector.EventID(ev.Source, ev.NativeID)
	human, refusal := h.person(app, in)
	if refusal != "" {
		return refusal
	}
	view, err := l2.NewView(ctx, l2.New(h.db), h.repo, human)
	if err != nil {
		log.ErrorContext(ctx, "reading as a discord user's principal", "principal", human.ID, "error", err)
		return "Hearsay could not read the graph as you, so it did nothing."
	}
	if in.Subcommand == discord.CommandPin {
		return h.pin(ctx, app, in, view, human, event)
	}
	return h.merge(ctx, in, view, human)
}

// person is the configured human a Discord user maps to, or the refusal that
// explains why there is none.
func (h *Interactions) person(app *discord.App, in discord.Interaction) (principal.Principal, string) {
	res := h.resolver.Resolve(app.Author(in))
	switch res.Status {
	case principal.Resolved:
	case principal.Ambiguous:
		return principal.Principal{}, fmt.Sprintf("Your Discord account maps to more than one Hearsay principal (%s), so Hearsay did nothing. Ask whoever configures Hearsay to fix the mapping.", strings.Join(res.Candidates, ", "))
	default:
		return principal.Principal{}, "Your Discord account is not mapped to a Hearsay principal, so Hearsay did nothing. Ask whoever configures Hearsay to add it to your principal's identities."
	}
	if res.Principal.Kind != principal.KindHuman {
		return principal.Principal{}, fmt.Sprintf("Your Discord account maps to %q, which is a %s. Only a person can pin or merge.", res.Principal.ID, res.Principal.Kind)
	}
	return res.Principal, ""
}

// pin pins the thread the command was run in: its L1 document, as an anchor
// in its scope.
func (h *Interactions) pin(ctx context.Context, app *discord.App, in discord.Interaction, view *l2.View, human principal.Principal, event string) string {
	if !in.Channel.IsThread() {
		return "Run `/hearsay pin` inside a thread. It pins the thread's distilled document, and this channel is not a thread."
	}
	const undistilled = "Hearsay has not distilled this thread yet, so there is nothing to pin. Try again once it has."
	doc := l1.DocID(app.Source, discord.ThreadArtifact(in.Channel.ID))
	got, err := l1.New(h.db).Get(ctx, doc)
	switch {
	case errors.Is(err, l1.ErrNotFound):
		return undistilled
	case err != nil:
		telemetry.Logger(ctx).ErrorContext(ctx, "reading a thread to pin", "document", doc, "error", err)
		return "Hearsay could not read this thread, so it pinned nothing. Try again."
	case !view.Reader().MayRead(got.Document):
		return undistilled
	}
	g, recorded, err := l2.RecordGesture(ctx, h.db, h.repo, l2.GestureRequest{Event: event, Principal: human.ID, Action: l2.GesturePin, Documents: []string{doc}})
	if err != nil {
		return refused(ctx, "pin this thread", err)
	}
	if !recorded {
		return fmt.Sprintf("This command already pinned the thread in scope %s, as gesture %d.", g.Scope, g.ID)
	}
	return fmt.Sprintf("Pinned this thread in scope %s, as gesture %d. Undo it with `hearsay gestures undo %d`.", g.Scope, g.ID, g.ID)
}

// merge merges one topic the person may read into another.
func (h *Interactions) merge(ctx context.Context, in discord.Interaction, view *l2.View, human principal.Principal) string {
	from, into := in.Options[discord.OptionFrom], in.Options[discord.OptionInto]
	if from == into {
		return "Pick two different topics: a topic cannot be merged into itself."
	}
	names := map[string]string{}
	for _, id := range []string{from, into} {
		a, err := view.Topic(ctx, id)
		if err != nil {
			telemetry.Logger(ctx).ErrorContext(ctx, "reading a topic to merge", "topic", id, "error", err)
			return "Hearsay could not read the topics, so it merged nothing. Try again."
		}
		if a == nil {
			return fmt.Sprintf("Hearsay has no topic %q that you can read, so it merged nothing. Pick both topics from the list Discord offers.", id)
		}
		names[id] = a.Topic.Name
	}
	op, err := l2.Operate(ctx, h.db, h.repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: human.ID, From: from, Into: into})
	if err != nil {
		return refused(ctx, "merge these topics", err)
	}
	return fmt.Sprintf("Merged %q into %q in scope %s, as operation %d. Undo it with `hearsay topics undo %d`.", names[from], names[into], op.Scope, op.ID, op.ID)
}

// refused explains an error from the ledger: a refusal says why, and
// anything else is logged and answered as a failure.
func refused(ctx context.Context, what string, err error) string {
	switch {
	case errors.Is(err, l2.ErrNotAllowed):
		return fmt.Sprintf("You may not %s: %v.", what, err)
	case errors.Is(err, l2.ErrInvalid), errors.Is(err, l2.ErrNotFound), errors.Is(err, l2.ErrConflict):
		return fmt.Sprintf("Hearsay could not %s: %v.", what, err)
	}
	telemetry.Logger(ctx).ErrorContext(ctx, "applying a discord command", "error", err)
	return fmt.Sprintf("Hearsay could not %s because of an internal error, and changed nothing. Try again.", what)
}

// choices are the topics a `/hearsay merge` argument may take: those the
// person may read whose name or id holds what they have typed. For `into`,
// once `from` names a topic they may read, only the other topics in its
// scope, since a merge stays inside one scope. Anyone Hearsay cannot map to
// a person is offered nothing, and so is a request out of time.
func (h *Interactions) choices(ctx context.Context, app *discord.App, in discord.Interaction) []discord.Choice {
	if in.Guild != app.Guild || in.Command != discord.CommandName || in.Subcommand != discord.CommandMerge ||
		(in.Focused != discord.OptionFrom && in.Focused != discord.OptionInto) {
		return nil
	}
	human, refusal := h.person(app, in)
	if refusal != "" {
		return nil
	}
	log := telemetry.Logger(ctx)
	view, err := l2.NewView(ctx, l2.New(h.db), h.repo, human)
	if err != nil {
		log.ErrorContext(ctx, "reading as a discord user's principal", "principal", human.ID, "error", err)
		return nil
	}
	typed := strings.ToLower(strings.TrimSpace(in.Options[in.Focused]))
	scopes := l2.ScopeKeys(h.repo)
	exclude := ""
	if in.Focused == discord.OptionInto {
		if from := in.Options[discord.OptionFrom]; from != "" {
			a, err := view.Topic(ctx, from)
			if err != nil {
				log.ErrorContext(ctx, "reading a topic to merge", "topic", from, "error", err)
				return nil
			}
			if a != nil {
				scopes, exclude = []string{a.Topic.Scope}, a.Topic.ID
			}
		}
	}
	var out []discord.Choice
	for _, scope := range scopes {
		topics, err := view.TopicsWhere(ctx, scope, func(t l2.Topic) bool {
			return t.ID != exclude && (typed == "" || strings.Contains(strings.ToLower(t.Name), typed) || strings.Contains(t.ID, typed))
		})
		if err != nil {
			if ctx.Err() == nil {
				log.ErrorContext(ctx, "listing topics to merge", "scope", scope, "error", err)
			}
			break
		}
		for _, a := range topics {
			out = append(out, discord.Choice{Name: a.Topic.Name + " · " + scope, Value: a.Topic.ID})
		}
		if len(out) >= discord.MaxChoices {
			break
		}
	}
	return out
}

package discord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The application command Hearsay registers, and its two subcommands.
const (
	CommandName = "hearsay"
	// CommandPin pins the thread it is run in.
	CommandPin = "pin"
	// CommandMerge merges topic OptionFrom into topic OptionInto.
	CommandMerge = "merge"
	OptionFrom   = "from"
	OptionInto   = "into"
)

// InteractionType is what Discord is asking for (the interaction object's
// `type`).
type InteractionType int

// The interaction types Hearsay answers.
const (
	InteractionPing         InteractionType = 1
	InteractionCommand      InteractionType = 2
	InteractionAutocomplete InteractionType = 4
)

// MaxChoices is the most autocomplete choices Discord shows.
const MaxChoices = 25

// maxAnswer is the most characters a message holds.
const maxAnswer = 2000

// ephemeral is the message flag that shows an answer only to the person who
// ran the command.
const ephemeral = 1 << 6

// Command is an application command as Discord registers it.
type Command struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Type        int             `json:"type,omitempty"`
	Options     []CommandOption `json:"options,omitempty"`
}

// CommandOption is a subcommand or an argument of one.
type CommandOption struct {
	Type         int             `json:"type"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Required     bool            `json:"required,omitempty"`
	Autocomplete bool            `json:"autocomplete,omitempty"`
	Options      []CommandOption `json:"options,omitempty"`
}

// Commands are the application commands Hearsay registers in a guild:
// `/hearsay pin`, run in a thread, and `/hearsay merge`, whose two topics are
// picked through autocomplete.
func Commands() []Command {
	const subcommand, text = 1, 3
	return []Command{{
		Name: CommandName, Description: "Correct what Hearsay knows", Type: 1,
		Options: []CommandOption{
			{Type: subcommand, Name: CommandPin, Description: "Pin this thread as an anchor in its scope"},
			{Type: subcommand, Name: CommandMerge, Description: "Merge one topic into another", Options: []CommandOption{
				{Type: text, Name: OptionFrom, Description: "The topic to merge away", Required: true, Autocomplete: true},
				{Type: text, Name: OptionInto, Description: "The topic it goes into", Required: true, Autocomplete: true},
			}},
		},
	}}
}

// App is a source's Discord application as the API runtime's interaction
// adapter uses it (ADR-0022): the public key its interactions are verified
// with, and the bot token that registers its commands. The connector never
// uses it; the connector writes L0 only.
type App struct {
	// Source is the source id the application belongs to.
	Source string
	// Guild is the guild its commands are registered in and answered from.
	Guild string
	// ID is the application id.
	ID    string
	key   ed25519.PublicKey
	token string
	api   string
	http  *http.Client
}

// NewApp returns the application a source configures, or nil where its
// settings name none or the source is read-only: such a source takes no
// commands. The source's secrets must be resolved.
func NewApp(src connector.SourceConfig) (*App, error) {
	var s Settings
	if err := src.DecodeSettings(&s); err != nil {
		return nil, err
	}
	key, ok, err := s.app()
	if err != nil || !ok || src.ReadOnly {
		return nil, err
	}
	if !snowflake(s.Guild) {
		return nil, errors.New("discord setting guild must be a snowflake")
	}
	token := src.Secrets[SecretToken]
	if token == "" {
		return nil, errors.New("discord secret token is required to register commands")
	}
	api, err := apiURL(s.APIURL)
	if err != nil {
		return nil, err
	}
	return &App{Source: src.ID, Guild: s.Guild, ID: s.ApplicationID, key: key, token: token, api: api, http: &http.Client{Timeout: 10 * time.Second}}, nil
}

// HasApp reports whether a source's settings name an application the API
// answers, before its secrets are resolved: a source that names none, or is
// read-only, needs no token outside the connectors process.
func HasApp(src connector.SourceConfig) bool {
	var s Settings
	return !src.ReadOnly && src.DecodeSettings(&s) == nil && (s.ApplicationID != "" || s.PublicKey != "")
}

// app validates the application settings, which are set together or not at
// all, and returns the public key.
func (s Settings) app() (ed25519.PublicKey, bool, error) {
	if s.ApplicationID == "" && s.PublicKey == "" {
		return nil, false, nil
	}
	if !snowflake(s.ApplicationID) {
		return nil, false, errors.New("discord setting application_id must be a snowflake, set together with public_key")
	}
	key, err := hex.DecodeString(s.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, false, errors.New("discord setting public_key must be the application's hex public key, set together with application_id")
	}
	return ed25519.PublicKey(key), true, nil
}

// Verify reports whether a request carries Discord's signature over its
// timestamp and body, made with the application's key. Discord requires an
// endpoint to refuse a request that does not.
func (a *App) Verify(h http.Header, body []byte) bool {
	sig, err := hex.DecodeString(h.Get("X-Signature-Ed25519"))
	stamp := h.Get("X-Signature-Timestamp")
	if err != nil || len(sig) != ed25519.SignatureSize || stamp == "" {
		return false
	}
	return ed25519.Verify(a.key, append([]byte(stamp), body...), sig)
}

// Register replaces the application's commands in the guild with
// [Commands], using the bot token. It is idempotent, so it runs at every
// start.
func (a *App) Register(ctx context.Context) error {
	body, err := json.Marshal(Commands())
	if err != nil {
		return err
	}
	return a.send(ctx, http.MethodPut, a.api+"/applications/"+a.ID+"/guilds/"+a.Guild+"/commands", body, true)
}

// Edit replaces the deferred answer to an interaction. It uses the
// interaction's token, not the bot's, and so can write nothing but that
// answer. A 404 is retried: the deferral may not have reached Discord yet.
func (a *App) Edit(ctx context.Context, token, content string) error {
	// The deferral already made the answer ephemeral; an edit sets no flags.
	edit := ephemeralAnswer(content)
	edit.Flags = 0
	body, err := json.Marshal(edit)
	if err != nil {
		return err
	}
	target := a.api + "/webhooks/" + a.ID + "/" + url.PathEscape(token) + "/messages/@original"
	for attempt := 1; ; attempt++ {
		err = a.send(ctx, http.MethodPatch, target, body, false)
		var status statusError
		if err == nil || attempt == 3 || !errors.As(err, &status) || (status != http.StatusNotFound && status < 500) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("discord answered %d", int(e)) }

// send makes one REST call. An error never carries the URL, which may hold an
// interaction token.
func (a *App) send(ctx context.Context, method, target string, body []byte, bot bool) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("building a discord request")
	}
	req.Header.Set("Content-Type", "application/json")
	if bot {
		req.Header.Set("Authorization", "Bot "+a.token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		var u *url.Error
		if errors.As(err, &u) {
			err = u.Err
		}
		return fmt.Errorf("calling discord: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return statusError(resp.StatusCode)
	}
	return nil
}

// Interaction is what Hearsay reads of an interaction Discord sends.
type Interaction struct {
	ID    string
	Type  InteractionType
	Token string
	Guild string
	// Channel is where it was run: a thread's own id, type and parent.
	Channel InteractionChannel
	User    InteractionUser
	// Command and Subcommand are the command run, or being filled in.
	Command, Subcommand string
	// Options are the subcommand's arguments, as typed so far for an
	// autocomplete.
	Options map[string]string
	// Focused is the argument an autocomplete is for.
	Focused string
}

// InteractionChannel is the channel an interaction was run in.
type InteractionChannel struct {
	ID       string `json:"id"`
	Type     int    `json:"type"`
	ParentID string `json:"parent_id"`
}

// IsThread reports whether the channel is a thread.
func (c InteractionChannel) IsThread() bool { return c.Type == 10 || c.Type == 11 || c.Type == 12 }

// Container is the allowlisted channel the interaction's channel is, or is a
// thread of: the container its command event is ingested under.
func (c InteractionChannel) Container() string {
	if c.IsThread() {
		return c.ParentID
	}
	return c.ID
}

// InteractionUser is who ran it.
type InteractionUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Global   string `json:"global_name"`
	Nick     string `json:"-"`
}

type interactionOption struct {
	Name    string              `json:"name"`
	Type    int                 `json:"type"`
	Value   json.RawMessage     `json:"value"`
	Focused bool                `json:"focused"`
	Options []interactionOption `json:"options"`
}

// ParseInteraction reads an interaction's body.
func ParseInteraction(body []byte) (Interaction, error) {
	var raw struct {
		ID        string             `json:"id"`
		Type      InteractionType    `json:"type"`
		Token     string             `json:"token"`
		GuildID   string             `json:"guild_id"`
		ChannelID string             `json:"channel_id"`
		Channel   InteractionChannel `json:"channel"`
		Member    *struct {
			Nick string           `json:"nick"`
			User *InteractionUser `json:"user"`
		} `json:"member"`
		User *InteractionUser `json:"user"`
		Data struct {
			Name    string              `json:"name"`
			Options []interactionOption `json:"options"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Interaction{}, fmt.Errorf("reading a discord interaction: %w", err)
	}
	if raw.Type != InteractionPing && !snowflake(raw.ID) {
		return Interaction{}, errors.New("a discord interaction has no id")
	}
	in := Interaction{ID: raw.ID, Type: raw.Type, Token: raw.Token, Guild: raw.GuildID, Channel: raw.Channel,
		Command: raw.Data.Name, Options: map[string]string{}}
	if in.Channel.ID == "" {
		in.Channel.ID = raw.ChannelID
	}
	switch {
	case raw.Member != nil && raw.Member.User != nil:
		in.User = *raw.Member.User
		in.User.Nick = raw.Member.Nick
	case raw.User != nil:
		in.User = *raw.User
	}
	options := raw.Data.Options
	if len(options) == 1 && options[0].Type == 1 {
		in.Subcommand = options[0].Name
		options = options[0].Options
	}
	for _, o := range options {
		var s string
		if json.Unmarshal(o.Value, &s) != nil {
			s = string(o.Value)
		}
		in.Options[o.Name] = s
		if o.Focused {
			in.Focused = o.Name
		}
	}
	return in, nil
}

// Author is who ran the interaction, as a source identity.
func (a *App) Author(in Interaction) connector.Identity {
	display := in.User.Nick
	if display == "" {
		display = in.User.Global
	}
	if display == "" {
		display = in.User.Username
	}
	return connector.Identity{Source: a.Source, Kind: connector.IdentityUser, NativeID: in.User.ID, Handle: in.User.Username, DisplayName: display}
}

// CommandEvent is the L0 `command` event a command is recorded as: who ran
// what, where. Its id is the interaction's, so a request delivered twice is
// one event. It is control traffic, never distilled, and is readable only
// through the channel it was run in.
func (a *App) CommandEvent(in Interaction) connector.Event {
	native, _ := json.Marshal(struct {
		Command    string            `json:"command"`
		Subcommand string            `json:"subcommand"`
		Options    map[string]string `json:"options,omitempty"`
		Channel    string            `json:"channel"`
		Thread     bool              `json:"thread,omitempty"`
	}{in.Command, in.Subcommand, in.Options, in.Channel.ID, in.Channel.IsThread()})
	text := "/" + strings.TrimSpace(in.Command+" "+in.Subcommand)
	for _, name := range []string{OptionFrom, OptionInto} {
		if v, ok := in.Options[name]; ok {
			text += " " + name + ":" + v
		}
	}
	author := a.Author(in)
	artifact := "interaction:" + in.ID
	return connector.Event{
		Source: a.Source, NativeID: artifact, Kind: connector.KindCommand, Time: snowflakeTime(in.ID),
		ACL: connector.ACL{{Kind: connector.ACLGroup, Source: a.Source, NativeID: in.Channel.ID}},
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: connector.Container{Kind: connector.ContainerChannel, NativeID: in.Channel.Container()},
			Text:      text, Author: &author, URL: channelURL(a.Guild, in.Channel.ID), Native: native,
		},
	}
}

// ThreadArtifact is the artifact id a thread's events carry in
// `payload.thread`, and so the key of the thread's L1 document.
func ThreadArtifact(id string) string { return threadArtifact(id) }

// Response is an answer to an interaction.
type Response struct {
	Type int `json:"type"`
	Data any `json:"data,omitempty"`
}

type answer struct {
	Content         string   `json:"content"`
	Flags           int      `json:"flags,omitempty"`
	AllowedMentions mentions `json:"allowed_mentions"`
}

// mentions pings nobody.
type mentions struct {
	Parse []string `json:"parse"`
}

func ephemeralAnswer(content string) answer {
	if r := []rune(content); len(r) > maxAnswer {
		content = string(r[:maxAnswer-1]) + "…"
	}
	return answer{Content: content, Flags: ephemeral, AllowedMentions: mentions{Parse: []string{}}}
}

// Choice is one autocomplete choice: what the person sees and the value the
// option takes.
type Choice struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Pong answers Discord's check of the endpoint.
func Pong() Response { return Response{Type: 1} }

// Answer is an ephemeral answer, seen only by the person who ran the command.
func Answer(content string) Response { return Response{Type: 4, Data: ephemeralAnswer(content)} }

// Defer tells Discord an ephemeral answer follows, through [App.Edit].
func Defer() Response {
	return Response{Type: 5, Data: struct {
		Flags int `json:"flags"`
	}{ephemeral}}
}

// Choices answers an autocomplete. At most [MaxChoices] are sent. A name is
// cut to the 100 characters Discord takes; a value longer than that is left
// out, since a cut value would name something else.
func Choices(choices []Choice) Response {
	out := make([]Choice, 0, min(len(choices), MaxChoices))
	for _, c := range choices {
		if len(out) == MaxChoices {
			break
		}
		if len([]rune(c.Value)) <= 100 {
			out = append(out, Choice{Name: cut(c.Name, 100), Value: c.Value})
		}
	}
	return Response{Type: 8, Data: struct {
		Choices []Choice `json:"choices"`
	}{out}}
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

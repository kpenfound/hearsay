package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// CommandWord starts a comment that is a command to Hearsay: `/hearsay ratify`,
// `/hearsay demote`, `/hearsay pin`, `/hearsay merge <topic-id> <topic-id>`.
const CommandWord = "/hearsay"

// KindReply is the extension kind of a comment Hearsay wrote to answer a
// command. It behaves like a `command`: control traffic, never distilled
// (ADR-0022).
const KindReply connector.Kind = "github.reply"

// replyMarker starts the invisible line every reply ends with. It names the
// command comment the reply answers, which is how the webhook echo of a reply
// is told from a person's comment and how a retried reply finds the one it
// already posted.
const replyMarker = "<!-- hearsay:reply comment="

// ReplyMarker is the line a reply to command comment id carries.
func ReplyMarker(id int64) string { return replyMarker + strconv.FormatInt(id, 10) + " -->" }

// replyTo is the command comment a reply names, and false for a comment that
// is not a reply.
func replyTo(body string) (int64, bool) {
	_, rest, ok := strings.Cut(body, replyMarker)
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, " -->")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	return id, err == nil && id > 0
}

// ParseCommand reads a comment as a command: the first line that is not blank
// starts with [CommandWord] as a word of its own. The name is the word after
// it, lower-cased, and empty for `/hearsay` alone; args are the words after
// that, with backticks around them taken off, since a person pasting a topic
// id from `hearsay topics list` often quotes it. Lines after the first are not
// read. A comment that does not start that way is not a command, whatever it
// says further down.
func ParseCommand(body string) (name string, args []string, ok bool) {
	var line string
	for l := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	words := strings.Fields(line)
	if len(words) == 0 || words[0] != CommandWord {
		return "", nil, false
	}
	if len(words) > 1 {
		name = strings.ToLower(words[1])
	}
	for _, w := range words[min(2, len(words)):] {
		if w = strings.Trim(w, "`"); w != "" {
			args = append(args, w)
		}
	}
	return name, args, true
}

// Command is a `/hearsay` comment as the connector describes it in L0: the
// `native` of a `command` event.
type Command struct {
	// Comment is the comment's id, which a reply names.
	Comment int64 `json:"id"`
	// Repo and Issue are the repository and the number of the issue or pull
	// request it was written on.
	Repo  string `json:"repository"`
	Issue int    `json:"issue"`
	// Name and Args are what [ParseCommand] read from its text.
	Name string   `json:"command"`
	Args []string `json:"args,omitempty"`
	// Edited is a revision written after the comment was: GitHub moved its
	// `updated_at` past its `created_at`. Hearsay runs a command as it was
	// written and never an edit of one.
	Edited bool `json:"edited,omitempty"`
}

// CommandOf reads the command a `command` event of a GitHub source describes.
func CommandOf(ev connector.Event) (Command, error) {
	if ev.Kind != connector.KindCommand {
		return Command{}, fmt.Errorf("event %s is a %s, not a command", ev.ID, ev.Kind)
	}
	var cmd Command
	if err := json.Unmarshal(ev.Payload.Native, &cmd); err != nil {
		return Command{}, fmt.Errorf("reading command %s: %w", ev.ID, err)
	}
	if cmd.Comment <= 0 || cmd.Issue <= 0 || cmd.Repo == "" {
		return Command{}, fmt.Errorf("command %s names no comment, repository and issue", ev.ID)
	}
	return cmd, nil
}

type replyNative struct {
	ID      int64 `json:"id"`
	ReplyTo int64 `json:"reply_to"`
}

// controlEvent turns a comment's event into the control traffic it is, where
// it is: Hearsay's reply to a command, which is never a command itself, and a
// `/hearsay` command. Anything else is left a `message`.
func controlEvent(ev connector.Event, cm comment, issue int, repo string) (connector.Event, error) {
	if to, ok := replyTo(cm.Body); ok {
		raw, err := json.Marshal(replyNative{ID: cm.ID, ReplyTo: to})
		if err != nil {
			return connector.Event{}, fmt.Errorf("encoding reply %d: %w", cm.ID, err)
		}
		ev.Kind, ev.Payload.BaseKind, ev.Payload.Native = KindReply, connector.KindCommand, raw
		return ev, nil
	}
	name, args, ok := ParseCommand(cm.Body)
	if !ok {
		return ev, nil
	}
	raw, err := json.Marshal(Command{
		Comment: cm.ID, Repo: repo, Issue: issue, Name: name, Args: args,
		Edited: cm.UpdatedAt.After(cm.CreatedAt),
	})
	if err != nil {
		return connector.Event{}, fmt.Errorf("encoding command %d: %w", cm.ID, err)
	}
	ev.Kind, ev.Payload.Native = connector.KindCommand, raw
	return ev, nil
}

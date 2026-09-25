package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// slashCommand is Slack's Socket Mode slash_commands payload. Slack's
// documented payload has no thread_ts, but clients that supply it can pin
// directly; a message permalink is also accepted as a pin argument.
type slashCommand struct {
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	Command     string `json:"command"`
	Text        string `json:"text"`
	ThreadTS    string `json:"thread_ts"`
	TriggerID   string `json:"trigger_id"`
	ResponseURL string `json:"response_url"`
}

// parseCommand only accepts the configured workspace and a public channel.
// The trigger id identifies one invocation across Socket Mode redelivery.
func (c *Connector) parseCommand(raw json.RawMessage) (connector.Command, string, bool) {
	var in slashCommand
	if json.Unmarshal(raw, &in) != nil || in.TeamID != c.team || !c.configured(in.ChannelID) ||
		in.UserID == "" || in.TriggerID == "" || in.Command != "/hearsay" || !responseURL(in.ResponseURL) {
		return connector.Command{}, "", false
	}
	words := strings.Fields(in.Text)
	if len(words) == 0 || (words[0] != connector.CommandPin && words[0] != connector.CommandMerge) {
		return connector.Command{}, "", false
	}
	verb := words[0]
	artifact := "command:" + url.QueryEscape(in.TriggerID)
	author := connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: in.UserID, Handle: in.UserName}
	at := commandTime(in.TriggerID)
	ev := c.event(connector.KindCommand, artifact, in.ChannelID, at)
	ev.Payload.Author = &author
	ev.Payload.Text = "/hearsay " + strings.TrimSpace(in.Text)
	ev.Payload.URL = "https://slack.com/archives/" + in.ChannelID
	cmd := connector.Command{Event: ev, Verb: verb}
	if verb == connector.CommandPin {
		ts := in.ThreadTS
		if ts == "" && len(words) > 1 {
			ts = pinTimestamp(in.ChannelID, words[1])
		}
		if _, ok := tsTime(ts); ok {
			cmd.Target = messageArtifact(in.ChannelID, ts)
		}
	} else if len(words) >= 3 {
		cmd.From = strings.TrimPrefix(words[1], "from:")
		cmd.Into = strings.TrimPrefix(words[2], "into:")
	}
	return cmd, in.ResponseURL, true
}

func commandTime(trigger string) time.Time {
	first, _, _ := strings.Cut(trigger, ".")
	if at, ok := tsTime(first + ".000000"); ok {
		return at
	}
	return time.Unix(1, 0).UTC()
}

func pinTimestamp(channel, input string) string {
	u, err := url.Parse(input)
	if err == nil && u.Scheme == "https" && u.Host == "slack.com" {
		if thread := u.Query().Get("thread_ts"); thread != "" {
			if _, ok := tsTime(thread); ok {
				return thread
			}
		}
		part := "/archives/" + channel + "/p"
		if digits, ok := strings.CutPrefix(u.Path, part); ok && len(digits) > 6 {
			return digits[:len(digits)-6] + "." + digits[len(digits)-6:]
		}
	}
	return input
}

// A response_url is a short-lived capability. Never send one to an arbitrary
// address from a forged fixture or a malformed envelope.
func responseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return false
	}
	return u.Scheme == "https" && (u.Host == "hooks.slack.com" || strings.HasSuffix(u.Host, ".slack.com")) ||
		u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")
}

func (c *Connector) respond(ctx context.Context, target, verb string, result connector.CommandResult, commandErr error) {
	msg := commandAnswer(result, verb)
	if commandErr != nil {
		msg = "Hearsay could not process this command. Try again."
	}
	body, _ := json.Marshal(struct {
		ResponseType string `json:"response_type"`
		Text         string `json:"text"`
	}{"ephemeral", msg})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}
}

func commandAnswer(r connector.CommandResult, verb string) string {
	switch r.Outcome {
	case connector.CommandPinned, connector.CommandAlreadyPinned:
		return fmt.Sprintf("Pinned this thread in scope %s as gesture %d.", r.Scope, r.Gesture)
	case connector.CommandMerged:
		return fmt.Sprintf("Merged %q into %q in scope %s as operation %d.", r.FromName, r.IntoName, r.Scope, r.Operation)
	case connector.CommandNoSuchTopic:
		return fmt.Sprintf("Hearsay has no topic %q that you can read, so it merged nothing.", r.Topic)
	case connector.CommandNotAllowed:
		return "You may not " + verb + ": " + r.Reason
	case connector.CommandUnmapped, connector.CommandAmbiguous, connector.CommandNotHuman:
		return "Your Slack account is not mapped to one authorized human principal, so Hearsay did nothing."
	case connector.CommandNoTarget:
		return "Give `/hearsay pin` a thread message link so Hearsay knows which thread to pin."
	case connector.CommandUndistilled:
		return "Hearsay has not distilled this thread yet, so there is nothing to pin."
	case connector.CommandNotRead:
		return "Hearsay does not read this channel, so it takes no commands here."
	case connector.CommandSameTopic:
		return "Pick two different topics."
	case connector.CommandUnknown:
		return "Hearsay has `/hearsay pin` and `/hearsay merge`."
	}
	return "Hearsay could not " + verb + " this time and changed nothing. Try again."
}

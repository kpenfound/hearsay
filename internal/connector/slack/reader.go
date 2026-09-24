package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// User is a Slack workspace member with an email visible to the bot token.
type User struct {
	ID      string
	Email   string
	Deleted bool
	Bot     bool
}

// ErrEmailScope means directory matching was skipped because the bot token
// cannot read workspace email addresses.
var ErrEmailScope = errors.New("slack bot token lacks users:read or users:read.email")

// Reader makes the optional directory reads used by hearsay init. It needs
// only the bot token; the app-level Socket Mode token is not used here.
type Reader struct {
	connector *Connector
	token     string
}

// NewReader constructs a Slack directory reader with a bot token and API URL.
func NewReader(token, api string) (*Reader, error) {
	if !strings.HasPrefix(token, "xoxb-") {
		return nil, fmt.Errorf("slack directory needs a bot token (xoxb-…)")
	}
	base, err := apiURL(api)
	if err != nil {
		return nil, err
	}
	return &Reader{connector: &Connector{api: base, http: &http.Client{Timeout: 30 * time.Second}}, token: token}, nil
}

// PublicChannel checks the same class restrictions as the runtime. A channel
// the app is not yet in can be configured, but must be invited before ingest.
func (r *Reader) PublicChannel(ctx context.Context, id string) error {
	if err := channelClass(id); err != nil {
		return err
	}
	var out struct {
		response
		Channel channelInfo `json:"channel"`
	}
	if err := r.connector.call(ctx, "conversations.info", r.token, url.Values{"channel": {id}}, &out); err != nil {
		return err
	}
	if why := out.Channel.unsupported(); why != "" {
		return fmt.Errorf("slack channel %s %s: only public channels are ingested", id, why)
	}
	if out.Channel.ID != id {
		return fmt.Errorf("slack channel %s returned id %s", id, out.Channel.ID)
	}
	return nil
}

// Users lists every page. Empty email fields are ignored by identity matching.
func (r *Reader) Users(ctx context.Context) ([]User, error) {
	var users []User
	cursor := ""
	seen := map[string]bool{}
	for {
		var out struct {
			response
			Members []struct {
				ID      string `json:"id"`
				Deleted bool   `json:"deleted"`
				IsBot   bool   `json:"is_bot"`
				Profile struct {
					Email string `json:"email"`
				} `json:"profile"`
			} `json:"members"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		form := url.Values{"limit": {"200"}}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		if err := r.connector.call(ctx, "users.list", r.token, form, &out); err != nil {
			var api *apiError
			if errors.As(err, &api) && api.code == "missing_scope" {
				return nil, ErrEmailScope
			}
			return nil, err
		}
		for _, m := range out.Members {
			users = append(users, User{ID: m.ID, Email: m.Profile.Email, Deleted: m.Deleted, Bot: m.IsBot})
		}
		cursor = out.Metadata.NextCursor
		if cursor == "" {
			return users, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("slack users.list repeated cursor %q", cursor)
		}
		seen[cursor] = true
	}
}

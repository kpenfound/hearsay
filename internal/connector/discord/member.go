package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// restStatus is a REST call Discord answered with something other than
// success. It prints as the status line, which is what the walks have always
// reported.
type restStatus int

func (e restStatus) Error() string { return fmt.Sprintf("%d %s", int(e), http.StatusText(int(e))) }

// Member is one guild member, as the member lookup returns them.
type Member struct {
	ID       string
	Username string
	Bot      bool
}

// Member looks one user up in the source's guild. The second result is false
// when Discord says the user is not a member. It is what `hearsay init`
// confirms a Discord account with before it writes it on a principal, and
// nothing else calls it: a single-member lookup needs no privileged intent,
// where listing the guild's members would need GUILD_MEMBERS.
func (c *Connector) Member(ctx context.Context, userID string) (Member, bool, error) {
	if !snowflake(userID) {
		return Member{}, false, fmt.Errorf("discord user %q is not a snowflake", userID)
	}
	var m member
	err := c.get(ctx, "/guilds/"+c.guild+"/members/"+userID, &m)
	if status := restStatus(0); errors.As(err, &status) && status == http.StatusNotFound {
		return Member{}, false, nil
	}
	if err != nil {
		return Member{}, false, err
	}
	if m.User == nil || m.User.ID != userID {
		return Member{}, false, fmt.Errorf("discord GET /guilds/%s/members/%s: the answer is not that member", c.guild, userID)
	}
	return Member{ID: m.User.ID, Username: m.User.Username, Bot: m.User.Bot}, true, nil
}

// ProfileUserID reads a Discord user id out of a profile link, such as the
// `https://discord.com/users/<id>` a person lists among their accounts on
// another site. Anything else is not one.
func ProfileUserID(link string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	switch strings.ToLower(u.Host) {
	case "discord.com", "www.discord.com", "discordapp.com", "www.discordapp.com":
	default:
		return "", false
	}
	id, ok := strings.CutPrefix(strings.TrimSuffix(u.Path, "/"), "/users/")
	if !ok || !snowflake(id) {
		return "", false
	}
	return id, true
}

package onboard

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/connector/drive"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/connector/slack"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Directory is what `hearsay init` can read about the team from the sources.
// Each field is nil when there are no credentials for that source, or it is
// skipped, and then nothing is read from it.
type Directory struct {
	GitHub  GitHubReader
	Discord DiscordReader
	Drive   DriveReader
	Slack   SlackReader
}

// GitHubReader is what seeding reads from GitHub: [github.Reader].
type GitHubReader interface {
	Collaborators(ctx context.Context, repo string) ([]github.Collaborator, error)
	Profile(ctx context.Context, login string) (github.Profile, error)
	SocialAccounts(ctx context.Context, login string) ([]string, error)
}

// DiscordReader is what seeding reads from Discord: [discord.Connector].
type DiscordReader interface {
	Member(ctx context.Context, userID string) (discord.Member, bool, error)
}

// DriveReader is what seeding reads from Drive: [drive.Connector].
type DriveReader interface {
	FolderUsers(ctx context.Context, folder string) ([]drive.User, error)
}

// SlackReader is the directory surface init needs from a Slack bot token.
type SlackReader interface {
	PublicChannel(ctx context.Context, id string) error
	Users(ctx context.Context) ([]slack.User, error)
}

// person is one human principal being written.
type person struct {
	id, name string
	// login is their GitHub login, when GitHub said they are a collaborator.
	login string
	// email is the address their GitHub profile makes public, which is what
	// a Drive account is matched on.
	email                            string
	github, discord, slackID, gdrive *principal.Identity
	operator                         bool
}

func (p *person) identities() []principal.Identity {
	var out []principal.Identity
	for _, id := range []*principal.Identity{p.github, p.discord, p.slackID, p.gdrive} {
		if id != nil {
			out = append(out, *id)
		}
	}
	return out
}

// people seeds the human principals: the operator, as they described
// themselves, and with GitHub credentials every collaborator of the
// repositories, with a Discord or Drive identity only where the sources
// confirm one without a guess. notes says what was left out and why.
func people(ctx context.Context, in Input, dir Directory) (all []*person, notes []string, err error) {
	op := &person{id: in.Operator.ID, name: in.Operator.Name, operator: true}
	if in.Operator.GitHub != "" {
		op.github = &principal.Identity{Source: SourceGitHub, Handle: in.Operator.GitHub}
	}
	if in.Operator.Discord != "" {
		op.discord = &principal.Identity{Source: SourceDiscord, NativeID: in.Operator.Discord}
	}
	if in.Operator.Slack != "" {
		op.slackID = &principal.Identity{Source: SourceSlack, NativeID: in.Operator.Slack}
	}
	if in.Operator.Email != "" {
		op.gdrive = &principal.Identity{Source: SourceDrive, Handle: in.Operator.Email}
	}
	all = []*person{op}

	if dir.GitHub != nil && in.HasGitHub() {
		seen := map[string]bool{}
		for _, repo := range in.GitHub.Repos {
			collaborators, err := dir.GitHub.Collaborators(ctx, repo)
			if err != nil {
				return nil, nil, err
			}
			for _, c := range collaborators {
				key := strings.ToLower(c.Login)
				if seen[key] || c.Login == "" {
					continue
				}
				seen[key] = true
				if c.Bot || strings.HasSuffix(key, "[bot]") {
					notes = append(notes, fmt.Sprintf("github: %s is a bot, and is not seeded as a person", c.Login))
					continue
				}
				profile, err := dir.GitHub.Profile(ctx, c.Login)
				if err != nil {
					return nil, nil, err
				}
				ident := &principal.Identity{Source: SourceGitHub, NativeID: c.NodeID, Handle: c.Login}
				if strings.EqualFold(c.Login, in.Operator.GitHub) {
					op.github, op.login, op.email = ident, c.Login, profile.Email
					if op.name == "" {
						op.name = profile.Name
					}
					continue
				}
				if key == op.id {
					return nil, nil, fmt.Errorf("GitHub collaborator %s would be principal %q, which is the id you chose for yourself: if that account is yours, give it as your GitHub login; otherwise choose another id", c.Login, key)
				}
				if !principal.ValidID(key) {
					notes = append(notes, fmt.Sprintf("github: %s makes no principal id, and is left out", c.Login))
					continue
				}
				all = append(all, &person{id: key, name: profile.Name, login: c.Login, email: profile.Email, github: ident})
			}
		}
		slices.SortFunc(all[1:], func(a, b *person) int { return strings.Compare(a.id, b.id) })

		if dir.Discord != nil && in.HasDiscord() {
			more, err := discordIdentities(ctx, all, dir)
			if err != nil {
				return nil, nil, err
			}
			notes = append(notes, more...)
		}
		if dir.Drive != nil && in.HasDrive() {
			more, err := driveIdentities(ctx, in, all, dir)
			if err != nil {
				return nil, nil, err
			}
			notes = append(notes, more...)
		}
	}
	if dir.Slack != nil && in.HasSlack() {
		for _, channel := range in.Slack.Channels {
			if err := dir.Slack.PublicChannel(ctx, channel); err != nil {
				return nil, nil, fmt.Errorf("checking Slack public channel %s: %w", channel, err)
			}
		}
		more, err := slackIdentities(ctx, all, dir.Slack)
		if err != nil {
			return nil, nil, err
		}
		notes = append(notes, more...)
	}
	return all, notes, nil
}

// slackIdentities joins only a unique email on both sides. The operator's
// stated Slack id is kept, but never assigned to somebody else as well.
func slackIdentities(ctx context.Context, all []*person, reader SlackReader) ([]string, error) {
	users, err := reader.Users(ctx)
	if errors.Is(err, slack.ErrEmailScope) {
		return []string{"slack: bot token has no users:read.email, so no Slack identities were matched"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing Slack users: %w", err)
	}
	claims := map[string][]*person{}
	for _, p := range all {
		email := p.email
		if p.operator && p.gdrive != nil {
			email = p.gdrive.Handle
		}
		if email != "" {
			claims[foldEmail(email)] = append(claims[foldEmail(email)], p)
		}
	}
	accounts := map[string][]slack.User{}
	for _, u := range users {
		if u.ID == "" || u.Email == "" || u.Deleted || u.Bot {
			continue
		}
		key := foldEmail(u.Email)
		if !slices.ContainsFunc(accounts[key], func(v slack.User) bool { return v.ID == u.ID }) {
			accounts[key] = append(accounts[key], u)
		}
	}
	var notes []string
	for _, email := range slices.Sorted(maps.Keys(accounts)) {
		ps, us := claims[email], accounts[email]
		if len(ps) == 0 {
			continue
		}
		if len(ps) != 1 || len(us) != 1 {
			notes = append(notes, fmt.Sprintf("slack: %s matches %d principals and %d Slack accounts, so no identity is added", email, len(ps), len(us)))
			continue
		}
		p, u := ps[0], us[0]
		if p.slackID != nil {
			if p.slackID.NativeID != u.ID {
				notes = append(notes, fmt.Sprintf("slack: %s matches %s, but you gave %s", email, u.ID, p.slackID.NativeID))
			}
			continue
		}
		if slices.ContainsFunc(all, func(other *person) bool { return other.slackID != nil && other.slackID.NativeID == u.ID }) {
			notes = append(notes, fmt.Sprintf("slack: account %s is already claimed, so it is not assigned by email", u.ID))
			continue
		}
		p.slackID = &principal.Identity{Source: SourceSlack, NativeID: u.ID}
	}
	return notes, nil
}

// discordIdentities adds the Discord account each collaborator links from
// their GitHub profile, where exactly one of them links it and it is a member
// of the guild. Discord shows a bot no email address and no linked accounts,
// so the link a person published on GitHub, confirmed by the guild, is the
// only match the sources give: a Discord username that happens to equal a
// GitHub login is no evidence (docs/config.md, "Matching is per source").
func discordIdentities(ctx context.Context, all []*person, dir Directory) ([]string, error) {
	var notes []string
	claims := map[string][]*person{}
	for _, p := range all {
		if p.login == "" {
			continue
		}
		links, err := dir.GitHub.SocialAccounts(ctx, p.login)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, link := range links {
			if id, ok := discord.ProfileUserID(link); ok && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) > 1 {
			notes = append(notes, fmt.Sprintf("discord: %s links %d Discord accounts on GitHub, so none is written", p.login, len(ids)))
			continue
		}
		for _, id := range ids {
			claims[id] = append(claims[id], p)
		}
	}

	for _, p := range all {
		if !p.operator || p.discord == nil {
			continue
		}
		// The operator's own word about their account is written as given; the
		// guild only adds the username to it.
		m, ok, err := dir.Discord.Member(ctx, p.discord.NativeID)
		if err != nil {
			return nil, err
		}
		if ok {
			p.discord.Handle = m.Username
		} else {
			notes = append(notes, fmt.Sprintf("discord: your account %s is not a member of the guild", p.discord.NativeID))
		}
	}

	for _, id := range slices.Sorted(maps.Keys(claims)) {
		claimants := claims[id]
		names := logins(claimants)
		switch {
		case len(claimants) > 1:
			notes = append(notes, fmt.Sprintf("discord: account %s is linked from GitHub by %s, so it is written on nobody", id, strings.Join(names, " and ")))
			continue
		case claimants[0].discord != nil:
			if claimants[0].discord.NativeID != id {
				notes = append(notes, fmt.Sprintf("discord: %s links account %s on GitHub, and you gave %s", names[0], id, claimants[0].discord.NativeID))
			}
			continue
		case discordTaken(all, id):
			notes = append(notes, fmt.Sprintf("discord: %s links account %s on GitHub, which you gave as yours", names[0], id))
			continue
		}
		m, ok, err := dir.Discord.Member(ctx, id)
		if err != nil {
			return nil, err
		}
		switch {
		case !ok:
			notes = append(notes, fmt.Sprintf("discord: %s links account %s on GitHub, which is not a member of the guild", names[0], id))
		case m.Bot:
			notes = append(notes, fmt.Sprintf("discord: %s links account %s on GitHub, which is a bot", names[0], id))
		default:
			claimants[0].discord = &principal.Identity{Source: SourceDiscord, NativeID: id, Handle: m.Username}
		}
	}
	return notes, nil
}

func discordTaken(all []*person, id string) bool {
	for _, p := range all {
		if p.discord != nil && p.discord.NativeID == id {
			return true
		}
	}
	return false
}

// driveIdentities adds the Drive account whose address a collaborator's
// GitHub profile makes public, where the folders are shared with exactly that
// address and no other collaborator publishes it. The operator's own address,
// if they gave one, stands for theirs.
func driveIdentities(ctx context.Context, in Input, all []*person, dir Directory) ([]string, error) {
	var notes []string
	shared := map[string][]drive.User{}
	for _, folder := range in.Drive.Folders {
		users, err := dir.Drive.FolderUsers(ctx, folder)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			key := foldEmail(u.Email)
			if !slices.ContainsFunc(shared[key], func(v drive.User) bool { return v.PermissionID == u.PermissionID }) {
				shared[key] = append(shared[key], u)
			}
		}
	}

	claims := map[string][]*person{}
	for _, p := range all {
		email := p.email
		if p.operator && p.gdrive != nil {
			email = p.gdrive.Handle
		}
		if email != "" {
			claims[foldEmail(email)] = append(claims[foldEmail(email)], p)
		}
	}
	for _, email := range slices.Sorted(maps.Keys(claims)) {
		claimants := claims[email]
		users := shared[email]
		switch {
		case len(users) == 0:
			continue
		case len(claimants) > 1:
			notes = append(notes, fmt.Sprintf("drive: %s is the address of %s, so it is written on nobody", email, strings.Join(logins(claimants), " and ")))
			continue
		case len(users) > 1:
			notes = append(notes, fmt.Sprintf("drive: %s is %d Drive accounts, so it is written on nobody", email, len(users)))
			continue
		}
		p := claimants[0]
		if p.gdrive != nil && foldEmail(p.gdrive.Handle) != email {
			continue
		}
		p.gdrive = &principal.Identity{Source: SourceDrive, NativeID: users[0].PermissionID, Handle: users[0].Email}
	}
	return notes, nil
}

func foldEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// logins names people for a note: the GitHub login, or the principal id of
// the operator who is not a collaborator.
func logins(ps []*person) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.login != "" {
			out = append(out, p.login)
		} else {
			out = append(out, p.id)
		}
	}
	slices.Sort(out)
	return out
}

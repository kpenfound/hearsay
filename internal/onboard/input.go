// Package onboard writes a new team's first configuration: the single-file
// `hearsay.yaml` and the env file holding the secrets it names, from what
// `hearsay init` asked for and what the sources say about who is on the team.
//
// It needs no database. The only IO is the reads a [Directory] makes of the
// sources a person has credentials for, and none of those credentials is
// written anywhere: the env file holds the API tokens generated here and
// empty entries for everything else.
package onboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/connector/drive"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/principal"
)

// The ids of the sources a generated configuration configures. A source id is
// chosen once, because it is in every event id, so these are the names the
// documentation's example uses rather than anything derived.
const (
	SourceGitHub  = "github"
	SourceDiscord = "discord"
	SourceDrive   = "drive"
)

// The environment variables the generated configuration names for source and
// model credentials. The env file carries each one empty, for the person to
// fill in.
const (
	EnvGitHubToken         = "HEARSAY_GITHUB_TOKEN"
	EnvGitHubWebhookSecret = "HEARSAY_GITHUB_WEBHOOK_SECRET"
	EnvDiscordToken        = "HEARSAY_DISCORD_TOKEN"
	EnvDriveCredentials    = "HEARSAY_DRIVE_CREDENTIALS"
	EnvAnthropicAPIKey     = "ANTHROPIC_API_KEY"
)

// Input is what a person answered: who they are, and what each source should
// ingest. A source with nothing to ingest is skipped.
type Input struct {
	Operator Operator
	GitHub   GitHub
	Discord  Discord
	Drive    Drive
}

// Operator is the person running `hearsay init`, who is a principal whether
// or not any source can be read. Every identity here is their own statement
// about themselves, and is written as given.
type Operator struct {
	// ID is their principal id.
	ID   string
	Name string
	// GitHub is their GitHub login.
	GitHub string
	// Discord is their Discord user id.
	Discord string
	// Email is the Google account address Drive shares with them.
	Email string
}

// GitHub is the GitHub source: the repositories, by full name. None skips it.
type GitHub struct {
	Repos []string
	// APIURL is a GitHub Enterprise Server's REST API; empty is github.com.
	APIURL string
}

// Discord is the Discord source: a guild and the parent channels in it to
// ingest. No guild skips it.
type Discord struct {
	Guild    string
	Channels []string
	// APIURL replaces Discord's REST API, for a local fixture.
	APIURL string
}

// Drive is the Drive source: the folders, by id. None skips it.
type Drive struct {
	Folders []string
	// APIURL replaces the Drive API, for a local fixture.
	APIURL string
}

// HasGitHub reports whether the GitHub source is configured.
func (in Input) HasGitHub() bool { return len(in.GitHub.Repos) > 0 }

// HasDiscord reports whether the Discord source is configured.
func (in Input) HasDiscord() bool { return in.Discord.Guild != "" }

// HasDrive reports whether the Drive source is configured.
func (in Input) HasDrive() bool { return len(in.Drive.Folders) > 0 }

var (
	githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	githubPart  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	snowflake   = regexp.MustCompile(`^[0-9]{1,20}$`)
	driveID     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// CheckPrincipalID says what is wrong with a principal id, or nothing.
func CheckPrincipalID(id string) error {
	if !principal.ValidID(id) {
		return fmt.Errorf("%q is not a principal id: 1-64 lowercase letters, digits, - and _, starting with a letter or a digit", id)
	}
	return nil
}

// CheckGitHubLogin says what is wrong with a GitHub login, or nothing.
func CheckGitHubLogin(login string) error {
	if !githubLogin.MatchString(login) || strings.Contains(login, "--") || strings.HasSuffix(login, "-") {
		return fmt.Errorf("%q is not a GitHub login", login)
	}
	return nil
}

// CheckRepo says what is wrong with a repository full name, or nothing.
func CheckRepo(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !githubPart.MatchString(owner) || !githubPart.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("%q is not a repository full name (owner/name)", repo)
	}
	return nil
}

// CheckSnowflake says what is wrong with a Discord id, or nothing. what is
// what the id is of, for the message.
func CheckSnowflake(what, id string) error {
	if !snowflake.MatchString(id) {
		return fmt.Errorf("%q is not a Discord %s id: Discord ids are digits (turn on Developer Mode, then Copy ID)", id, what)
	}
	return nil
}

// CheckFolder says what is wrong with a Drive folder id, or nothing.
func CheckFolder(id string) error {
	if !driveID.MatchString(id) {
		return fmt.Errorf("%q is not a Drive folder id: it is the last part of the folder's URL", id)
	}
	return nil
}

// CheckEmail says what is wrong with an email address, or nothing. It checks
// the shape a person could mistype, not deliverability.
func CheckEmail(email string) error {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.ContainsAny(email, " \t\n,;") {
		return fmt.Errorf("%q is not an email address", email)
	}
	return nil
}

// Check reports everything wrong with the input, not the first thing.
func (in Input) Check() error {
	var errs []error
	add := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
	}
	if in.Operator.ID == "" {
		errs = append(errs, errors.New("operator: is required: it is your principal id, such as your first name in lowercase"))
	} else {
		add("operator", CheckPrincipalID(in.Operator.ID))
	}
	if in.Operator.GitHub != "" {
		add("operator-github", CheckGitHubLogin(in.Operator.GitHub))
		if !in.HasGitHub() {
			errs = append(errs, errors.New("operator-github: names a GitHub account, and GitHub is skipped"))
		}
	}
	if in.Operator.Discord != "" {
		add("operator-discord", CheckSnowflake("user", in.Operator.Discord))
		if !in.HasDiscord() {
			errs = append(errs, errors.New("operator-discord: names a Discord account, and Discord is skipped"))
		}
	}
	if in.Operator.Email != "" {
		add("operator-email", CheckEmail(in.Operator.Email))
		if !in.HasDrive() {
			errs = append(errs, errors.New("operator-email: names a Drive account, and Drive is skipped"))
		}
	}
	for _, r := range in.GitHub.Repos {
		add("github-repo", CheckRepo(r))
	}
	if dup := duplicate(in.GitHub.Repos, strings.ToLower); dup != "" {
		errs = append(errs, fmt.Errorf("github-repo: %q is listed twice", dup))
	}
	if in.HasDiscord() {
		add("discord-guild", CheckSnowflake("guild", in.Discord.Guild))
		if len(in.Discord.Channels) == 0 {
			errs = append(errs, errors.New("discord-channel: name at least one channel to ingest, or skip Discord"))
		}
	} else if len(in.Discord.Channels) > 0 {
		errs = append(errs, errors.New("discord-channel: names channels, and Discord is skipped: give the guild too"))
	}
	for _, c := range in.Discord.Channels {
		add("discord-channel", CheckSnowflake("channel", c))
	}
	if dup := duplicate(in.Discord.Channels, nil); dup != "" {
		errs = append(errs, fmt.Errorf("discord-channel: %q is listed twice", dup))
	}
	for _, f := range in.Drive.Folders {
		add("drive-folder", CheckFolder(f))
	}
	if dup := duplicate(in.Drive.Folders, nil); dup != "" {
		errs = append(errs, fmt.Errorf("drive-folder: %q is listed twice", dup))
	}
	if !in.HasGitHub() && !in.HasDiscord() && !in.HasDrive() {
		errs = append(errs, errors.New("every source is skipped: a configuration needs at least one to ingest anything"))
	}
	if in.Operator.GitHub == "" && in.Operator.Discord == "" && in.Operator.Email == "" {
		errs = append(errs, errors.New("the operator needs an identity in a configured source: give your GitHub login, Discord user id or Drive email"))
	}
	return errors.Join(errs...)
}

// duplicate returns the first value listed twice, compared through fold when
// it is not nil.
func duplicate(values []string, fold func(string) string) string {
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		k := v
		if fold != nil {
			k = fold(v)
		}
		if seen[k] {
			return v
		}
		seen[k] = true
	}
	return ""
}

// source is one source the generated file configures, with its settings and
// secrets in the order the file writes them.
type source struct {
	id, typ    string
	containers []string
	settings   []pair
	secrets    []pair
}

// pair is one key of a mapping that is written in a fixed order.
type pair struct{ key, value string }

// sources are the sources the input asks for, in the order the generated file
// writes them.
func (in Input) sources() []source {
	var out []source
	if in.HasGitHub() {
		src := source{
			id: SourceGitHub, typ: github.Type, containers: in.GitHub.Repos,
			secrets: []pair{{github.SecretToken, EnvGitHubToken}, {github.SecretWebhook, EnvGitHubWebhookSecret}},
		}
		if in.GitHub.APIURL != "" {
			src.settings = []pair{{"api_url", in.GitHub.APIURL}}
		}
		out = append(out, src)
	}
	if in.HasDiscord() {
		src := source{
			id: SourceDiscord, typ: discord.Type, containers: in.Discord.Channels,
			settings: []pair{{"guild", in.Discord.Guild}},
			secrets:  []pair{{discord.SecretToken, EnvDiscordToken}},
		}
		if in.Discord.APIURL != "" {
			src.settings = append(src.settings, pair{"api_url", in.Discord.APIURL})
		}
		out = append(out, src)
	}
	if in.HasDrive() {
		src := source{
			id: SourceDrive, typ: drive.Type, containers: in.Drive.Folders,
			secrets: []pair{{drive.SecretCredentials, EnvDriveCredentials}},
		}
		if in.Drive.APIURL != "" {
			src.settings = []pair{{"api_url", in.Drive.APIURL}}
		}
		out = append(out, src)
	}
	return out
}

// Sources are the source configurations the input asks for, as the generated
// file writes them, with each secret naming its environment variable. The CLI
// resolves them to build the readers a [Directory] holds, so what init reads
// with is what the services will.
func (in Input) Sources() []connector.SourceConfig {
	var out []connector.SourceConfig
	for _, s := range in.sources() {
		src := connector.SourceConfig{ID: s.id, Type: s.typ, Containers: slices.Clone(s.containers), Secrets: map[string]string{}}
		if len(s.settings) > 0 {
			settings := make(map[string]string, len(s.settings))
			for _, p := range s.settings {
				settings[p.key] = p.value
			}
			// A map of strings always encodes.
			src.Settings, _ = json.Marshal(settings)
		}
		for _, p := range s.secrets {
			src.Secrets[p.key] = p.value
		}
		out = append(out, src)
	}
	return out
}

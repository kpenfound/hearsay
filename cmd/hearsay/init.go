package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/connector/drive"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/onboard"
)

// initIO is what `hearsay init` reads besides its flags: the answers to its
// prompts, whether there is a person at the other end to prompt, the
// environment the source credentials come from, and the randomness the API
// tokens are made of. Tests give it their own.
type initIO struct {
	stdin    io.Reader
	terminal bool
	lookup   func(string) (string, bool)
	random   io.Reader
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	terminal := false
	if fi, err := os.Stdin.Stat(); err == nil {
		terminal = fi.Mode()&os.ModeCharDevice != 0
	}
	return initTeam(ctx, args, initIO{stdin: os.Stdin, terminal: terminal, lookup: os.LookupEnv, random: rand.Reader}, stdout, stderr)
}

// question is one prompt of `hearsay init` and the flag that answers it
// instead. Answering the prompt and passing the flag with the same text are
// the same thing: that is what makes the flow scriptable.
type question struct {
	flag   string
	prompt string
	// list takes several values, comma-separated or with the flag repeated.
	list bool
	// required is asked again until it is answered.
	required bool
	// when is whether it is asked at all, given the answers so far. A flag
	// given for a question that is not asked still counts.
	when  func(onboard.Input) bool
	check func(string) error
	set   func(*onboard.Input, []string)
}

func initQuestions() []question {
	first := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return v[0]
	}
	return []question{
		{flag: "operator", prompt: "Your principal id, the name Hearsay knows you by (lowercase, such as kyle)", required: true,
			check: onboard.CheckPrincipalID, set: func(in *onboard.Input, v []string) { in.Operator.ID = first(v) }},
		{flag: "operator-name", prompt: "Your name (optional)",
			set: func(in *onboard.Input, v []string) { in.Operator.Name = first(v) }},
		{flag: "github-repo", prompt: "GitHub repositories to ingest, owner/name, comma-separated (empty skips GitHub)", list: true,
			check: onboard.CheckRepo, set: func(in *onboard.Input, v []string) { in.GitHub.Repos = v }},
		{flag: "operator-github", prompt: "Your GitHub login", when: onboard.Input.HasGitHub,
			check: onboard.CheckGitHubLogin, set: func(in *onboard.Input, v []string) { in.Operator.GitHub = first(v) }},
		{flag: "discord-guild", prompt: "Discord server (guild) id (empty skips Discord)",
			check: func(s string) error { return onboard.CheckSnowflake("guild", s) },
			set:   func(in *onboard.Input, v []string) { in.Discord.Guild = first(v) }},
		{flag: "discord-channel", prompt: "Discord channel ids to ingest, comma-separated; each takes its threads", list: true, required: true,
			when:  onboard.Input.HasDiscord,
			check: func(s string) error { return onboard.CheckSnowflake("channel", s) },
			set:   func(in *onboard.Input, v []string) { in.Discord.Channels = v }},
		{flag: "operator-discord", prompt: "Your Discord user id (optional)", when: onboard.Input.HasDiscord,
			check: func(s string) error { return onboard.CheckSnowflake("user", s) },
			set:   func(in *onboard.Input, v []string) { in.Operator.Discord = first(v) }},
		{flag: "drive-folder", prompt: "Google Drive folder ids to ingest, comma-separated (empty skips Drive)", list: true,
			check: onboard.CheckFolder, set: func(in *onboard.Input, v []string) { in.Drive.Folders = v }},
		{flag: "operator-email", prompt: "Your Google account email (optional)", when: onboard.Input.HasDrive,
			check: onboard.CheckEmail, set: func(in *onboard.Input, v []string) { in.Operator.Email = first(v) }},
	}
}

// answer is a question's flag: whether it was given, and what it said.
type answer struct {
	list   bool
	given  bool
	values []string
}

func (a *answer) String() string { return strings.Join(a.values, ",") }

func (a *answer) Set(v string) error {
	if !a.list || !a.given {
		a.values = nil
	}
	a.given = true
	a.values = append(a.values, splitAnswer(v, a.list)...)
	return nil
}

// splitAnswer reads one answer the way a prompt and a flag both take it:
// trimmed, and for a list, split on commas with the empty items dropped. An
// empty answer is no value, which is how a source is skipped.
func splitAnswer(v string, list bool) []string {
	if !list {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		return []string{v}
	}
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// initTeam is `hearsay init`: it asks what the team uses, reads who is on it
// from the sources it has credentials for, and writes the configuration and
// the env file for its secrets (docs/config.md). It needs no database.
func initTeam(ctx context.Context, args []string, env initIO, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hearsay init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "hearsay.yaml", "the configuration file to write")
	envFile := fs.String("env-file", "hearsay.env", "the env file to write the secrets the configuration names to, mode 0600")
	force := fs.Bool("force", false, "overwrite the configuration and the env file if they exist")
	noInput := fs.Bool("no-input", false, "never prompt, even on a terminal: what no flag gives is skipped")
	githubAPI := fs.String("github-api-url", "", "a GitHub Enterprise Server's REST API, such as https://ghe.example/api/v3")
	discordAPI := fs.String("discord-api-url", "", "replace Discord's REST API, for a local fixture")
	driveAPI := fs.String("drive-api-url", "", "replace the Google Drive API, for a local fixture")
	questions := initQuestions()
	answers := make([]*answer, len(questions))
	for i, q := range questions {
		answers[i] = &answer{list: q.list}
		fs.Var(answers[i], q.flag, q.prompt)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	in := onboard.Input{
		GitHub:  onboard.GitHub{APIURL: *githubAPI},
		Discord: onboard.Discord{APIURL: *discordAPI},
		Drive:   onboard.Drive{APIURL: *driveAPI},
	}
	interactive := env.terminal && !*noInput
	var reader *bufio.Reader
	if interactive {
		reader = bufio.NewReader(env.stdin)
	}
	for i, q := range questions {
		switch {
		case answers[i].given:
			q.set(&in, answers[i].values)
		case q.when != nil && !q.when(in):
		case interactive:
			values, err := ask(reader, stderr, q)
			if err != nil {
				return err
			}
			q.set(&in, values)
		}
	}
	if err := in.Check(); err != nil {
		return fmt.Errorf("%w\n(run `hearsay init --help` for the flags)", err)
	}

	dir, notes, err := initDirectory(in, env.lookup)
	if err != nil {
		return err
	}
	res, err := onboard.Generate(ctx, in, dir, env.random)
	if err != nil {
		return err
	}
	repo, err := onboard.Write(res, *out, *envFile, *force)
	if errors.Is(err, onboard.ErrExists) {
		return fmt.Errorf("%w: nothing was written. Pass --force to overwrite", err)
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "wrote %s, which validates: sources %s; scopes %s; principals %s\n",
		*out, strings.Join(sourceIDs(repo), ", "), strings.Join(scopeIDs(repo), ", "), strings.Join(res.Principals, ", "))
	fmt.Fprintf(stdout, "wrote %s, mode 0600: generated API tokens in %s\n", *envFile, strings.Join(res.Tokens, ", "))
	fmt.Fprintf(stdout, "fill in %s in %s before starting Hearsay; each has a note on where to get it\n", strings.Join(res.Empty, ", "), *envFile)
	if notes = append(notes, res.Notes...); len(notes) > 0 {
		fmt.Fprintln(stdout, "left out:")
		for _, n := range notes {
			fmt.Fprintf(stdout, "  %s\n", n)
		}
		fmt.Fprintln(stdout, "after the first ingest, `hearsay identities list` names the accounts no principal claims")
	}
	return nil
}

// ask puts one question to the person at the terminal until it has an answer
// that passes its check. Running out of input answers an optional question
// with nothing, and fails a required one.
func ask(r *bufio.Reader, w io.Writer, q question) ([]string, error) {
	for {
		fmt.Fprintf(w, "%s [--%s]: ", q.prompt, q.flag)
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("reading the answer to --%s: %w", q.flag, err)
		}
		eof := err != nil
		if eof {
			fmt.Fprintln(w)
		}
		values := splitAnswer(line, q.list)
		var problems []string
		for _, v := range values {
			if q.check != nil {
				if cerr := q.check(v); cerr != nil {
					problems = append(problems, cerr.Error())
				}
			}
		}
		switch {
		case len(problems) > 0 && !eof:
			fmt.Fprintf(w, "  %s\n", strings.Join(problems, "\n  "))
		case len(problems) > 0:
			return nil, fmt.Errorf("--%s: %s", q.flag, strings.Join(problems, "; "))
		case len(values) == 0 && q.required && eof:
			return nil, fmt.Errorf("--%s is required, and the input ended before it was answered", q.flag)
		case len(values) == 0 && q.required:
			fmt.Fprintln(w, "  this one needs an answer")
		default:
			return values, nil
		}
	}
}

// initDirectory builds a reader for each configured source whose credential
// is in the environment, under the variable the generated configuration names
// for it, so what init reads with is what the services will. A source without
// one is not read, and the notes say what that leaves out. Nothing read here
// is written anywhere.
func initDirectory(in onboard.Input, lookup func(string) (string, bool)) (onboard.Directory, []string, error) {
	var dir onboard.Directory
	var notes []string
	for _, src := range in.Sources() {
		var name, env string
		switch src.Type {
		case github.Type:
			name, env = github.SecretToken, onboard.EnvGitHubToken
		case discord.Type:
			name, env = discord.SecretToken, onboard.EnvDiscordToken
		case drive.Type:
			name, env = drive.SecretCredentials, onboard.EnvDriveCredentials
		}
		value, ok := lookup(env)
		if !ok || value == "" {
			switch src.Type {
			case github.Type:
				notes = append(notes, fmt.Sprintf("github: %s is not set, so the collaborators were not read and you are the only principal", env))
			case discord.Type:
				if dir.GitHub != nil {
					notes = append(notes, fmt.Sprintf("discord: %s is not set, so no collaborator's Discord account was confirmed", env))
				}
			case drive.Type:
				if dir.GitHub != nil {
					notes = append(notes, fmt.Sprintf("drive: %s is not set, so no collaborator's Drive account was confirmed", env))
				}
			}
			continue
		}
		src.Secrets = map[string]string{name: value}
		if err := buildReader(&dir, src); err != nil {
			return onboard.Directory{}, nil, fmt.Errorf("source %q, with the credential in %s: %w", src.ID, env, err)
		}
	}
	return dir, notes, nil
}

func buildReader(dir *onboard.Directory, src connector.SourceConfig) error {
	switch src.Type {
	case github.Type:
		r, err := github.NewReader(src)
		if err != nil {
			return err
		}
		dir.GitHub = r
	case discord.Type:
		c, err := discord.New(src)
		if err != nil {
			return err
		}
		dir.Discord = c
	case drive.Type:
		c, err := drive.New(src)
		if err != nil {
			return err
		}
		dir.Drive = c
	}
	return nil
}

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
)

// fixtures serves one source's recorded API answers from testdata/init/<name>:
// a request for /a/b is the file a/b.json, page N of a paged list is
// a/b.pageN.json, and a path with no file is a 404, as GitHub and Discord
// answer one. Every request must carry the credential init was given.
type fixtures struct {
	*httptest.Server
	auth string

	mu   sync.Mutex
	seen []string
}

func serveFixtures(t *testing.T, name, auth string) *fixtures {
	t.Helper()
	root := filepath.Join("testdata", "init", name)
	f := &fixtures{auth: auth}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/token" {
			// Drive's service account exchange: any signed assertion will do.
			_ = r.ParseForm()
			if r.PostForm.Get("assertion") == "" {
				http.Error(w, "no assertion", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"access_token": "drive-access", "expires_in": 3600, "token_type": "Bearer"}`)
			return
		}
		if r.Header.Get("Authorization") != f.auth {
			http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		file := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		if name == "slack" && r.URL.Path == "/conversations.info" {
			file += "." + r.FormValue("channel")
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		body, err := os.ReadFile(pageFile(file, page))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message": "Unknown Member", "code": 10007}`)
			return
		}
		if _, err := os.Stat(pageFile(file, max(page, 1)+1)); err == nil {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(max(page, 1)+1))
			w.Header().Set("Link", `<`+f.URL+r.URL.Path+"?"+q.Encode()+`>; rel="next"`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.Close)
	return f
}

func pageFile(file string, page int) string {
	if page > 1 {
		return file + ".page" + strconv.Itoa(page) + ".json"
	}
	return file + ".json"
}

func (f *fixtures) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

// driveCredentials is a service account key for the Drive fixture: a real RSA
// key, made for the test, whose token endpoint is the fixture's.
func driveCredentials(t *testing.T, tokenURL string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "hearsay@acme-project.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":    tokenURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(cred)
}

// sources is one set of fixtures for the three sources, and the environment
// that holds the credentials for all of them.
type sources struct {
	github, discord, drive *fixtures
	env                    map[string]string
}

func newSources(t *testing.T) sources {
	s := sources{
		github:  serveFixtures(t, "github", "Bearer gh-token"),
		discord: serveFixtures(t, "discord", "Bot discord-token"),
		drive:   serveFixtures(t, "drive", "Bearer drive-access"),
	}
	s.env = map[string]string{
		"HEARSAY_GITHUB_TOKEN":      "gh-token",
		"HEARSAY_DISCORD_TOKEN":     "discord-token",
		"HEARSAY_DRIVE_CREDENTIALS": driveCredentials(t, s.drive.URL+"/token"),
	}
	return s
}

// apis are the flags that point init at the fixtures instead of the sources.
func (s sources) apis() []string {
	return []string{"--github-api-url", s.github.URL, "--discord-api-url", s.discord.URL, "--drive-api-url", s.drive.URL}
}

// teamFlags answer every question for a GitHub + Discord + Drive team.
var teamFlags = []string{
	"--operator", "kyle",
	"--github-repo", "acme/api,acme/infra",
	"--operator-github", "kpenfound",
	"--discord-guild", "824100000000000000",
	"--discord-channel", "824100000000000001", "--discord-channel", "824100000000000002",
	"--drive-folder", "1EngFolder,1MeetingsFolder",
}

// teamAnswers are the same answers typed at the prompts, one line each, in
// the order init asks. The name is left empty, as it is by the flags.
const teamAnswers = "kyle\n\nacme/api, acme/infra\nkpenfound\n824100000000000000\n824100000000000001,824100000000000002\n\n\n1EngFolder,1MeetingsFolder\n\n"

// sequence is a reproducible stand-in for crypto/rand: every byte is the
// next one, so tokens differ from each other and are the same on every run.
type sequence struct{ next byte }

func (s *sequence) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = s.next
		s.next++
	}
	return len(p), nil
}

type initCall struct {
	args     []string
	stdin    string
	terminal bool
	env      map[string]string
	random   io.Reader
}

// runInitIn runs `hearsay init` writing into dir, and returns what it printed.
func runInitIn(t *testing.T, dir string, c initCall) (string, error) {
	t.Helper()
	args := append([]string{"init", "--out", filepath.Join(dir, "hearsay.yaml"), "--env-file", filepath.Join(dir, "hearsay.env")}, c.args...)
	random := c.random
	if random == nil {
		random = &sequence{}
	}
	env := initIO{
		stdin:    strings.NewReader(c.stdin),
		terminal: c.terminal,
		lookup:   func(k string) (string, bool) { v, ok := c.env[k]; return v, ok },
		random:   random,
	}
	var stdout, stderr bytes.Buffer
	err := initTeam(t.Context(), args[1:], env, &stdout, &stderr)
	return stdout.String() + stderr.String(), err
}

// readEnv parses the env file into its variables, in order.
func readEnv(t *testing.T, path string) ([]string, map[string]string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var keys []string
	vars := map[string]string{}
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: line %q is not KEY=value", path, line)
		}
		keys = append(keys, k)
		vars[k] = v
	}
	return keys, vars
}

// identities is a principal as the test compares it: each identity as
// source/native_id/handle.
func identities(p principal.Principal) []string {
	var out []string
	for _, id := range p.Identities {
		out = append(out, id.Source+"/"+id.NativeID+"/"+id.Handle)
	}
	return out
}

var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hearsay init writes a configuration that loads, and an env file holding a
// generated token for every principal it wrote and nothing else but empty
// entries — across the ways a team can answer it and the credentials it can
// have.
func TestInitWritesAValidConfiguration(t *testing.T) {
	type person struct {
		name       string
		identities []string
	}
	full := map[string]person{
		"kyle": {"Kyle Penfound", []string{
			"github/MDQ6VXNlcjE=/kpenfound",
			"discord/302100000000000003/kyle",
			"drive/01234567890123456789/kyle@acme.example",
		}},
		// alex and jordan both link Discord account ...6 from GitHub, and
		// both publish shared@acme.example: neither is written on either.
		"alex":   {"Alex Doe", []string{"github/MDQ6VXNlcjQ=/alex"}},
		"jordan": {"Jordan Roe", []string{"github/MDQ6VXNlcjU=/jordan"}},
		// Listed by both repositories, as RobinOK by one: one principal. The
		// Drive address matches whatever its case.
		"robinok": {"Robin Okonkwo", []string{
			"github/MDQ6VXNlcjI=/robinok",
			"discord/302100000000000004/robin",
			"drive/11234567890123456789/robin@acme.example",
		}},
		// Links a Discord account that is not in the guild.
		"sam-r": {"", []string{"github/MDQ6VXNlcjM=/sam-r"}},
	}
	operatorOnly := func(ids ...string) map[string]person { return map[string]person{"kyle": {"", ids}} }

	tests := []struct {
		name string
		args []string
		// noCredentials runs with an empty environment.
		noCredentials bool
		// only keeps these credentials.
		only []string

		wantSources []string
		wantScopes  []string
		want        map[string]person
		// wantEmpty are the variables left for the person to fill in.
		wantEmpty []string
		// wantOutput are lines init prints.
		wantOutput []string
	}{
		{
			name:        "every source, every credential",
			args:        teamFlags,
			wantSources: []string{"github", "discord", "drive"},
			wantScopes:  []string{"api", "infra"},
			want:        full,
			wantEmpty:   []string{"HEARSAY_GITHUB_TOKEN", "HEARSAY_GITHUB_WEBHOOK_SECRET", "HEARSAY_DISCORD_TOKEN", "HEARSAY_DRIVE_CREDENTIALS", "ANTHROPIC_API_KEY"},
			wantOutput: []string{
				"github: dependabot[bot] is a bot, and is not seeded as a person",
				"discord: account 302100000000000006 is linked from GitHub by alex and jordan, so it is written on nobody",
				"discord: sam-r links account 302100000000000005 on GitHub, which is not a member of the guild",
				"drive: shared@acme.example is the address of alex and jordan, so it is written on nobody",
			},
		},
		{
			name:          "no credentials: the operator alone, as they described themselves",
			args:          append(slices.Clone(teamFlags), "--operator-discord", "302100000000000003", "--operator-email", "kyle@acme.example"),
			noCredentials: true,
			wantSources:   []string{"github", "discord", "drive"},
			wantScopes:    []string{"api", "infra"},
			want: operatorOnly(
				"github//kpenfound",
				"discord/302100000000000003/",
				"drive//kyle@acme.example",
			),
			wantEmpty:  []string{"HEARSAY_GITHUB_TOKEN", "HEARSAY_GITHUB_WEBHOOK_SECRET", "HEARSAY_DISCORD_TOKEN", "HEARSAY_DRIVE_CREDENTIALS", "ANTHROPIC_API_KEY"},
			wantOutput: []string{"github: HEARSAY_GITHUB_TOKEN is not set, so the collaborators were not read and you are the only principal"},
		},
		{
			// Discord and Drive can confirm an account, but only for someone
			// GitHub named: without it there is nobody to match.
			name:        "no GitHub credential",
			args:        teamFlags,
			only:        []string{"HEARSAY_DISCORD_TOKEN", "HEARSAY_DRIVE_CREDENTIALS"},
			wantSources: []string{"github", "discord", "drive"},
			wantScopes:  []string{"api", "infra"},
			want:        operatorOnly("github//kpenfound"),
			wantEmpty:   []string{"HEARSAY_GITHUB_TOKEN", "HEARSAY_GITHUB_WEBHOOK_SECRET", "HEARSAY_DISCORD_TOKEN", "HEARSAY_DRIVE_CREDENTIALS", "ANTHROPIC_API_KEY"},
		},
		{
			name:        "GitHub alone: no Discord or Drive account is confirmed",
			args:        teamFlags,
			only:        []string{"HEARSAY_GITHUB_TOKEN"},
			wantSources: []string{"github", "discord", "drive"},
			wantScopes:  []string{"api", "infra"},
			want: map[string]person{
				"kyle":    {"Kyle Penfound", []string{"github/MDQ6VXNlcjE=/kpenfound"}},
				"alex":    full["alex"],
				"jordan":  full["jordan"],
				"robinok": {"Robin Okonkwo", []string{"github/MDQ6VXNlcjI=/robinok"}},
				"sam-r":   full["sam-r"],
			},
			wantEmpty: []string{"HEARSAY_GITHUB_TOKEN", "HEARSAY_GITHUB_WEBHOOK_SECRET", "HEARSAY_DISCORD_TOKEN", "HEARSAY_DRIVE_CREDENTIALS", "ANTHROPIC_API_KEY"},
			wantOutput: []string{
				"discord: HEARSAY_DISCORD_TOKEN is not set, so no collaborator's Discord account was confirmed",
				"drive: HEARSAY_DRIVE_CREDENTIALS is not set, so no collaborator's Drive account was confirmed",
			},
		},
		{
			name:        "Discord and Drive skipped",
			args:        []string{"--operator", "kyle", "--operator-name", "Kyle P", "--github-repo", "acme/api", "--operator-github", "KPenfound"},
			wantSources: []string{"github"},
			wantScopes:  []string{"api"},
			want: map[string]person{
				// The name given wins over the profile's; the login is
				// written as GitHub spells it.
				"kyle":    {"Kyle P", []string{"github/MDQ6VXNlcjE=/kpenfound"}},
				"alex":    full["alex"],
				"robinok": {"Robin Okonkwo", []string{"github/MDQ6VXNlcjI=/robinok"}},
				"sam-r":   full["sam-r"],
			},
			wantEmpty: []string{"HEARSAY_GITHUB_TOKEN", "HEARSAY_GITHUB_WEBHOOK_SECRET", "ANTHROPIC_API_KEY"},
		},
		{
			name: "GitHub skipped: one scope over what is left",
			args: []string{"--operator", "kyle", "--discord-guild", "824100000000000000", "--discord-channel", "824100000000000001",
				"--operator-discord", "302100000000000003", "--drive-folder", "1EngFolder"},
			wantSources: []string{"discord", "drive"},
			wantScopes:  []string{"team"},
			want:        operatorOnly("discord/302100000000000003/"),
			wantEmpty:   []string{"HEARSAY_DISCORD_TOKEN", "HEARSAY_DRIVE_CREDENTIALS", "ANTHROPIC_API_KEY"},
		},
		{
			name:        "Drive alone",
			args:        []string{"--operator", "kyle", "--drive-folder", "1EngFolder", "--operator-email", "kyle@acme.example"},
			wantSources: []string{"drive"},
			wantScopes:  []string{"team"},
			want:        operatorOnly("drive//kyle@acme.example"),
			wantEmpty:   []string{"HEARSAY_DRIVE_CREDENTIALS", "ANTHROPIC_API_KEY"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSources(t)
			env := src.env
			switch {
			case tt.noCredentials:
				env = nil
			case tt.only != nil:
				env = map[string]string{}
				for _, k := range tt.only {
					env[k] = src.env[k]
				}
			}
			dir := t.TempDir()
			out, err := runInitIn(t, dir, initCall{args: append(src.apis(), tt.args...), env: env})
			if err != nil {
				t.Fatalf("init = %v\n%s", err, out)
			}
			for _, line := range tt.wantOutput {
				if !strings.Contains(out, line) {
					t.Errorf("output does not say %q:\n%s", line, out)
				}
			}

			repo, err := config.Load(filepath.Join(dir, "hearsay.yaml"))
			if err != nil {
				t.Fatalf("config.Load(hearsay.yaml) = %v", err)
			}
			if got := sourceIDs(repo); !slices.Equal(got, tt.wantSources) {
				t.Errorf("sources = %q, want %q", got, tt.wantSources)
			}
			if got := scopeIDs(repo); !slices.Equal(got, tt.wantScopes) {
				t.Errorf("scopes = %q, want %q", got, tt.wantScopes)
			}
			got := map[string]person{}
			for _, p := range repo.Principals {
				if p.Kind != principal.KindHuman {
					t.Errorf("principal %q is a %s, want a human", p.ID, p.Kind)
				}
				got[p.ID] = person{p.Name, identities(p)}
			}
			for id, w := range tt.want {
				if g, ok := got[id]; !ok {
					t.Errorf("no principal %q", id)
				} else if g.name != w.name || !slices.Equal(g.identities, w.identities) {
					t.Errorf("principal %q = %q %q, want %q %q", id, g.name, g.identities, w.name, w.identities)
				}
			}
			for id := range got {
				if _, ok := tt.want[id]; !ok {
					t.Errorf("principal %q was written, and is not wanted", id)
				}
			}
			if first := repo.Principals[0].ID; first != "kyle" {
				t.Errorf("the first principal is %q, want the operator", first)
			}

			// The env file: a distinct generated token under every
			// principal's token_env, the rest named in the configuration
			// and empty, and nothing else.
			keys, vars := readEnv(t, filepath.Join(dir, "hearsay.env"))
			var tokens []string
			for _, p := range repo.Principals {
				tok, ok := vars[p.TokenEnv]
				if !ok || !hexToken.MatchString(tok) {
					t.Errorf("principal %q: %s = %q in the env file, want 64 hex characters", p.ID, p.TokenEnv, tok)
				}
				if slices.Contains(tokens, tok) {
					t.Errorf("principal %q: token %s is another principal's too", p.ID, tok)
				}
				tokens = append(tokens, tok)
			}
			var empty []string
			for _, k := range keys {
				if vars[k] == "" {
					empty = append(empty, k)
				}
			}
			if !slices.Equal(empty, tt.wantEmpty) {
				t.Errorf("empty entries = %q, want %q", empty, tt.wantEmpty)
			}
			if len(keys) != len(repo.Principals)+len(tt.wantEmpty) {
				t.Errorf("env file holds %q: want a token per principal and the empty entries, and nothing else", keys)
			}
			for _, s := range repo.Sources {
				for _, v := range s.Secrets {
					if _, ok := vars[v]; !ok {
						t.Errorf("source %q names %s, which the env file does not hold", s.ID, v)
					}
				}
			}
			for _, secret := range src.env {
				if bytes.Contains(mustRead(t, filepath.Join(dir, "hearsay.env")), []byte(secret)) ||
					bytes.Contains(mustRead(t, filepath.Join(dir, "hearsay.yaml")), []byte(secret)) {
					t.Errorf("a credential init read with was written to a file")
				}
			}
			info, err := os.Stat(filepath.Join(dir, "hearsay.env"))
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("env file mode = %o, want 600", mode)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The generated file writes the documented defaults out, and they are the
// loader's: reading the file gives the authority and the model tiers a
// configuration that leaves both out gets.
func TestInitWritesTheLoaderDefaults(t *testing.T) {
	dir := t.TempDir()
	out, err := runInitIn(t, dir, initCall{args: []string{"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"}})
	if err != nil {
		t.Fatalf("init = %v\n%s", err, out)
	}
	generated, err := config.Load(filepath.Join(dir, "hearsay.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	bare := t.TempDir()
	body := mustRead(t, filepath.Join(dir, "hearsay.yaml"))
	cut := bytes.Index(body, []byte("\n# The built-in default"))
	if cut < 0 {
		t.Fatalf("hearsay.yaml has no authority section:\n%s", body)
	}
	if err := os.WriteFile(filepath.Join(bare, "hearsay.yaml"), body[:cut+1], 0o644); err != nil {
		t.Fatal(err)
	}
	defaults, err := config.Load(filepath.Join(bare, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("hearsay.yaml without authority and llm = %v", err)
	}
	g, d := generated.Authority.Default(), defaults.Authority.Default()
	if !slices.Equal(g.Ranking, d.Ranking) || !slices.Equal(g.RatifiedBy.Principals, d.RatifiedBy.Principals) ||
		!slices.Equal(g.RatifiedBy.Sources, d.RatifiedBy.Sources) || !slices.Equal(g.RatifiedBy.Artifacts, d.RatifiedBy.Artifacts) ||
		g.Window() != d.Window() {
		t.Errorf("generated authority = %+v, the loader's default is %+v", g, d)
	}
	if got, want := modelTiers(generated), modelTiers(defaults); !slices.Equal(got, want) {
		t.Errorf("generated model tiers = %q, the loader's defaults are %q", got, want)
	}
	for _, tier := range defaults.LLM.Configured() {
		gt, _ := generated.LLM.Tier(tier)
		dt, _ := defaults.LLM.Tier(tier)
		if gt != dt {
			t.Errorf("tier %s = %+v, the default is %+v", tier, gt, dt)
		}
	}
}

// Every prompt has a flag, and answering the prompts writes exactly what
// passing the flags does. A flag given on a terminal is not asked again.
func TestInitPromptsAndFlagsAgree(t *testing.T) {
	src := newSources(t)
	byFlags := t.TempDir()
	if out, err := runInitIn(t, byFlags, initCall{args: append(src.apis(), teamFlags...), env: src.env}); err != nil {
		t.Fatalf("init with flags = %v\n%s", err, out)
	}

	tests := []struct {
		name  string
		args  []string
		stdin string
		// wantAsked are the flags whose prompts were shown.
		wantAsked []string
	}{
		{
			name:      "every answer typed",
			stdin:     teamAnswers,
			wantAsked: []string{"operator", "operator-name", "github-repo", "operator-github", "discord-guild", "discord-channel", "operator-discord", "slack-workspace", "drive-folder", "operator-email"},
		},
		{
			name:  "a wrong answer is asked again",
			stdin: strings.Replace(teamAnswers, "acme/api, acme/infra\n", "acme\nacme/api, acme/infra\n", 1),
			wantAsked: []string{"operator", "operator-name", "github-repo", "github-repo", "operator-github", "discord-guild", "discord-channel",
				"operator-discord", "slack-workspace", "drive-folder", "operator-email"},
		},
		{
			name:      "flags for some, prompts for the rest",
			args:      []string{"--operator", "kyle", "--github-repo", "acme/api", "--github-repo", "acme/infra", "--drive-folder", "1EngFolder,1MeetingsFolder"},
			stdin:     "\nkpenfound\n824100000000000000\n824100000000000001,824100000000000002\n\n\n\n",
			wantAsked: []string{"operator-name", "operator-github", "discord-guild", "discord-channel", "operator-discord", "slack-workspace", "operator-email"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			out, err := runInitIn(t, dir, initCall{args: append(src.apis(), tt.args...), stdin: tt.stdin, terminal: true, env: src.env})
			if err != nil {
				t.Fatalf("init = %v\n%s", err, out)
			}
			var asked []string
			for _, m := range regexp.MustCompile(`\[--([a-z-]+)\]: `).FindAllStringSubmatch(out, -1) {
				asked = append(asked, m[1])
			}
			if !slices.Equal(asked, tt.wantAsked) {
				t.Errorf("asked %q, want %q\n%s", asked, tt.wantAsked, out)
			}
			for _, name := range []string{"hearsay.yaml", "hearsay.env"} {
				if got, want := mustRead(t, filepath.Join(dir, name)), mustRead(t, filepath.Join(byFlags, name)); !bytes.Equal(got, want) {
					t.Errorf("%s from the prompts differs from the flags':\n%s\nwant:\n%s", name, got, want)
				}
			}
		})
	}
}

// With nobody at a terminal, or with --no-input, nothing is asked: a question
// no flag answers is skipped, and a required one is an error.
func TestInitWithoutATerminal(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		terminal bool
		wantErr  string
	}{
		{name: "no operator", args: []string{"--github-repo", "acme/api"}, wantErr: "operator: is required"},
		{name: "--no-input on a terminal", args: []string{"--no-input", "--github-repo", "acme/api"}, terminal: true, wantErr: "operator: is required"},
		{name: "every source skipped", args: []string{"--operator", "kyle"}, wantErr: "every source is skipped"},
		{name: "a skipped source's flag", args: []string{"--operator", "kyle", "--drive-folder", "f1", "--operator-discord", "302100000000000003"},
			wantErr: "operator-discord: names a Discord account, and Discord is skipped"},
		{name: "a guild and no channel", args: []string{"--operator", "kyle", "--discord-guild", "824100000000000000", "--operator-discord", "302100000000000003"},
			wantErr: "discord-channel: name at least one channel"},
		{name: "no identity for the operator", args: []string{"--operator", "kyle", "--github-repo", "acme/api"},
			wantErr: "the operator needs an identity"},
		{name: "a malformed repository", args: []string{"--operator", "kyle", "--github-repo", "acme", "--operator-github", "kpenfound"},
			wantErr: `github-repo: "acme" is not a repository full name`},
		{name: "a principal id that is not one", args: []string{"--operator", "Kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"},
			wantErr: `operator: "Kyle" is not a principal id`},
		{name: "an argument", args: []string{"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound", "extra"},
			wantErr: `unexpected argument "extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			// The prompts would have an answer for everything, if they were shown.
			out, err := runInitIn(t, dir, initCall{args: tt.args, stdin: teamAnswers, terminal: tt.terminal})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("init = %v, want an error containing %q\n%s", err, tt.wantErr, out)
			}
			if strings.Contains(out, "[--") {
				t.Errorf("init prompted:\n%s", out)
			}
			if entries, _ := os.ReadDir(dir); len(entries) > 0 {
				t.Errorf("init wrote %v, want nothing", entries)
			}
		})
	}
}

// A collaborator whose login is the id the operator chose is refused rather
// than guessed to be them.
func TestInitRefusesAnOperatorIDThatIsSomeoneElsesLogin(t *testing.T) {
	src := newSources(t)
	dir := t.TempDir()
	out, err := runInitIn(t, dir, initCall{
		args: append(src.apis(), "--operator", "robinok", "--github-repo", "acme/api", "--operator-github", "kpenfound"),
		env:  src.env,
	})
	if err == nil || !strings.Contains(err.Error(), `GitHub collaborator robinok would be principal "robinok"`) {
		t.Fatalf("init = %v, want the collision refused\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("init wrote %v, want nothing", entries)
	}
}

// A credential the source refuses stops init, rather than writing a
// configuration that quietly seeded nobody.
func TestInitStopsOnARefusedCredential(t *testing.T) {
	src := newSources(t)
	src.env["HEARSAY_GITHUB_TOKEN"] = "wrong"
	dir := t.TempDir()
	out, err := runInitIn(t, dir, initCall{args: append(src.apis(), teamFlags...), env: src.env})
	if err == nil || !strings.Contains(err.Error(), "listing the collaborators of acme/api") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("init = %v, want the refusal\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("init wrote %v, want nothing", entries)
	}
}

// What init reads, and nothing more: the collaborators, one profile each, and
// the Discord and Drive lookups that confirm an account.
func TestInitReadsOnlyWhatItNeeds(t *testing.T) {
	src := newSources(t)
	if out, err := runInitIn(t, t.TempDir(), initCall{args: append(src.apis(), teamFlags...), env: src.env}); err != nil {
		t.Fatalf("init = %v\n%s", err, out)
	}
	wantGitHub := []string{
		"GET /repos/acme/api/collaborators", "GET /repos/acme/api/collaborators",
		"GET /users/kpenfound", "GET /users/robinok", "GET /users/sam-r", "GET /users/alex",
		"GET /repos/acme/infra/collaborators", "GET /users/jordan",
		"GET /users/kpenfound/social_accounts", "GET /users/alex/social_accounts", "GET /users/jordan/social_accounts",
		"GET /users/robinok/social_accounts", "GET /users/sam-r/social_accounts",
	}
	if got := src.github.requests(); !slices.Equal(got, wantGitHub) {
		t.Errorf("GitHub requests = %q, want %q", got, wantGitHub)
	}
	// The member lookups: the accounts kpenfound, robinok and sam-r link, in
	// id order. The one alex and jordan both link is not looked up.
	wantDiscord := []string{
		"GET /guilds/824100000000000000/members/302100000000000003",
		"GET /guilds/824100000000000000/members/302100000000000004",
		"GET /guilds/824100000000000000/members/302100000000000005",
	}
	if got := src.discord.requests(); !slices.Equal(got, wantDiscord) {
		t.Errorf("Discord requests = %q, want %q", got, wantDiscord)
	}
	wantDrive := []string{"POST /token", "GET /files/1EngFolder/permissions", "GET /files/1MeetingsFolder/permissions"}
	if got := src.drive.requests(); !slices.Equal(got, wantDrive) {
		t.Errorf("Drive requests = %q, want %q", got, wantDrive)
	}
}

// init refuses to overwrite the configuration or the env file, and then writes
// neither; with --force it replaces both, and the env file comes out 0600
// whatever it was.
func TestInitOverwrite(t *testing.T) {
	args := []string{"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"}
	tests := []struct {
		name     string
		existing []string // files that exist before init runs
		force    bool
		wantErr  string
	}{
		{name: "neither exists"},
		{name: "the configuration exists", existing: []string{"hearsay.yaml"}, wantErr: "hearsay.yaml already exists: nothing was written. Pass --force to overwrite"},
		{name: "the env file exists", existing: []string{"hearsay.env"}, wantErr: "hearsay.env already exists"},
		{name: "both exist", existing: []string{"hearsay.yaml", "hearsay.env"}, wantErr: "hearsay.yaml already exists"},
		{name: "--force replaces both", existing: []string{"hearsay.yaml", "hearsay.env"}, force: true},
		{name: "--force with nothing to replace", force: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			const old = "# somebody's file\n"
			for _, name := range tt.existing {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(old), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			call := slices.Clone(args)
			if tt.force {
				call = append(call, "--force")
			}
			out, err := runInitIn(t, dir, initCall{args: call})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("init = %v, want an error containing %q\n%s", err, tt.wantErr, out)
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != len(tt.existing) {
					t.Errorf("the directory holds %v, want only %q: nothing is written", entries, tt.existing)
				}
				for _, name := range tt.existing {
					if got := string(mustRead(t, filepath.Join(dir, name))); got != old {
						t.Errorf("%s = %q, want it untouched", name, got)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("init = %v\n%s", err, out)
			}
			if _, err := config.Load(filepath.Join(dir, "hearsay.yaml")); err != nil {
				t.Errorf("config.Load = %v", err)
			}
			for name, want := range map[string]os.FileMode{"hearsay.yaml": 0o644, "hearsay.env": 0o600} {
				info, err := os.Stat(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != want {
					t.Errorf("%s mode = %o, want %o", name, info.Mode().Perm(), want)
				}
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Errorf("the directory holds %v, want the two files and no leftovers", entries)
			}
		})
	}
}

// The configuration and the env file must be two files, and the
// configuration one the loader reads.
func TestInitRefusesTargetsThatCannotWork(t *testing.T) {
	args := []string{"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"}
	tests := []struct {
		name         string
		out, envFile string
		wantErr      string
	}{
		{name: "the same file", out: "hearsay.yaml", envFile: "hearsay.yaml", wantErr: "is both the configuration and the env file"},
		{name: "not YAML", out: "hearsay.conf", envFile: "hearsay.env", wantErr: "the configuration is a .yaml or .yml file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var stdout bytes.Buffer
			env := initIO{stdin: strings.NewReader(""), lookup: func(string) (string, bool) { return "", false }, random: &sequence{}}
			err := initTeam(t.Context(), append([]string{"--out", filepath.Join(dir, tt.out), "--env-file", filepath.Join(dir, tt.envFile)}, args...), env, &stdout, &stdout)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("init = %v, want an error containing %q", err, tt.wantErr)
			}
			if entries, _ := os.ReadDir(dir); len(entries) > 0 {
				t.Errorf("init wrote %v, want nothing", entries)
			}
		})
	}
}

// The tokens come from crypto/rand in the command, and a source of
// randomness that fails stops init before anything is written.
func TestInitTokens(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"}
	if out, err := runInitIn(t, dir, initCall{args: args, random: rand.Reader}); err != nil {
		t.Fatalf("init = %v\n%s", err, out)
	}
	_, vars := readEnv(t, filepath.Join(dir, "hearsay.env"))
	tok, err := hex.DecodeString(vars["HEARSAY_KYLE_TOKEN"])
	if err != nil || len(tok) != 32 {
		t.Errorf("HEARSAY_KYLE_TOKEN = %q, want 32 random bytes in hex", vars["HEARSAY_KYLE_TOKEN"])
	}

	failing := t.TempDir()
	_, err = runInitIn(t, failing, initCall{args: args, random: failingReader{}})
	if err == nil || !strings.Contains(err.Error(), "generating an API token") {
		t.Fatalf("init = %v, want the failure", err)
	}
	if entries, _ := os.ReadDir(failing); len(entries) > 0 {
		t.Errorf("init wrote %v, want nothing", entries)
	}
}

func TestInitSlackPromptsFlagsAndEmailMatches(t *testing.T) {
	github := serveFixtures(t, "github", "Bearer gh-token")
	slack := serveFixtures(t, "slack", "Bearer xoxb-fixture")
	base := []string{"--github-api-url", github.URL, "--slack-api-url", slack.URL,
		"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"}
	flags := append(slices.Clone(base), "--slack-workspace", "TWORK123", "--slack-channel", "CGOOD123")
	env := map[string]string{"HEARSAY_GITHUB_TOKEN": "gh-token", "HEARSAY_SLACK_BOT_TOKEN": "xoxb-fixture"}
	byFlags := t.TempDir()
	if out, err := runInitIn(t, byFlags, initCall{args: flags, env: env}); err != nil {
		t.Fatalf("flags: %v\n%s", err, out)
	}
	byPrompts := t.TempDir()
	answers := "\n\nTWORK123\nCGOOD123\n\n\n"
	out, err := runInitIn(t, byPrompts, initCall{args: base, env: env, terminal: true, stdin: answers})
	if err != nil {
		t.Fatalf("prompts: %v\n%s", err, out)
	}
	for _, name := range []string{"hearsay.yaml", "hearsay.env"} {
		if got, want := mustRead(t, filepath.Join(byPrompts, name)), mustRead(t, filepath.Join(byFlags, name)); !bytes.Equal(got, want) {
			t.Errorf("%s differs from flags", name)
		}
	}
	if !strings.Contains(out, "[--slack-workspace]") || !strings.Contains(out, "[--slack-channel]") {
		t.Errorf("Slack prompts absent: %s", out)
	}
	repo, err := config.Load(filepath.Join(byFlags, "hearsay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(repo); !slices.Equal(got, []string{"github", "slack"}) {
		t.Errorf("sources = %q", got)
	}
	if got := repo.Sources[1].Secrets; got["app_token"] != "HEARSAY_SLACK_APP_TOKEN" || got["bot_token"] != "HEARSAY_SLACK_BOT_TOKEN" {
		t.Errorf("Slack secrets = %v", got)
	}
	for _, scope := range repo.Scopes {
		if !slices.ContainsFunc(scope.Sources, func(s config.ScopeSource) bool { return s.Source == "slack" }) {
			t.Errorf("scope %s lacks Slack", scope.ID)
		}
	}
	want := map[string]string{"kyle": "UKYLE123", "robinok": "UROBIN12"}
	for _, person := range repo.Principals {
		var id string
		for _, ident := range person.Identities {
			if ident.Source == "slack" {
				id = ident.NativeID
			}
		}
		if id != want[person.ID] {
			t.Errorf("%s Slack identity = %q, want %q", person.ID, id, want[person.ID])
		}
	}
	_, vars := readEnv(t, filepath.Join(byFlags, "hearsay.env"))
	if vars["HEARSAY_SLACK_APP_TOKEN"] != "" || vars["HEARSAY_SLACK_BOT_TOKEN"] != "" {
		t.Errorf("Slack credentials were written: %v", vars)
	}
	if bytes.Contains(mustRead(t, filepath.Join(byFlags, "hearsay.env")), []byte("xoxb-fixture")) {
		t.Error("fetched credential was written")
	}
	if st, err := os.Stat(filepath.Join(byFlags, "hearsay.env")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("env mode: %v, %v", st, err)
	}
}

func TestInitSlackSkippedNoInputAndPrivateChannel(t *testing.T) {
	base := []string{"--operator", "kyle", "--github-repo", "acme/api", "--operator-github", "kpenfound"}
	if out, err := runInitIn(t, t.TempDir(), initCall{args: append([]string{"--no-input"}, base...), terminal: true}); err != nil || strings.Contains(out, "[--slack-") {
		t.Errorf("skipped Slack: %v\n%s", err, out)
	}
	bad := append(slices.Clone(base), "--slack-workspace", "TWORK123")
	if _, err := runInitIn(t, t.TempDir(), initCall{args: bad}); err == nil || !strings.Contains(err.Error(), "slack-channel") {
		t.Errorf("missing channels = %v", err)
	}
	bad = append(slices.Clone(base), "--slack-channel", "CGOOD123")
	if _, err := runInitIn(t, t.TempDir(), initCall{args: bad}); err == nil || !strings.Contains(err.Error(), "Slack is skipped") {
		t.Errorf("orphaned channel = %v", err)
	}
	slack := serveFixtures(t, "slack", "Bearer xoxb-fixture")
	bad = append(slices.Clone(base), "--slack-api-url", slack.URL, "--slack-workspace", "TWORK123", "--slack-channel", "CPRIVATE1")
	dir := t.TempDir()
	_, err := runInitIn(t, dir, initCall{args: bad, env: map[string]string{"HEARSAY_SLACK_BOT_TOKEN": "xoxb-fixture"}})
	if err == nil || !strings.Contains(err.Error(), "private channel") {
		t.Errorf("private channel = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote files for private channel: %v", entries)
	}
}

func TestInitSlackWithoutCredentialsOrGitHub(t *testing.T) {
	args := []string{"--operator", "kyle", "--operator-slack", "UKYLE123", "--slack-workspace", "TWORK123", "--slack-channel", "CGOOD123"}
	dir := t.TempDir()
	out, err := runInitIn(t, dir, initCall{args: args})
	if err != nil {
		t.Fatalf("Slack alone: %v\n%s", err, out)
	}
	repo, err := config.Load(filepath.Join(dir, "hearsay.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sourceIDs(repo), []string{"slack"}) || !slices.Equal(scopeIDs(repo), []string{"team"}) {
		t.Errorf("source/scope = %q/%q", sourceIDs(repo), scopeIDs(repo))
	}
	if got := identities(repo.Principals[0]); !slices.Equal(got, []string{"slack/UKYLE123/"}) {
		t.Errorf("operator identity = %q", got)
	}
	if !strings.Contains(out, "channels were not verified") {
		t.Errorf("missing credential note absent: %s", out)
	}
	_, vars := readEnv(t, filepath.Join(dir, "hearsay.env"))
	if _, ok := vars["HEARSAY_SLACK_APP_TOKEN"]; !ok {
		t.Error("app variable absent")
	}
	if _, ok := vars["HEARSAY_SLACK_BOT_TOKEN"]; !ok {
		t.Error("bot variable absent")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

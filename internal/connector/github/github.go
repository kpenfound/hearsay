// Package github is the GitHub connector: issues, issue and pull request
// comments, pull requests, reviews, review comments and commits on the default
// branch, as L0 events shaped exactly as the GitHub section of
// docs/connector-contract.md says.
//
// It is a [connector.Pusher], through GitHub's webhooks, a
// [connector.Backfiller], through the REST list endpoints, and a
// [connector.Resyncer], through the same walk over one repository. It never
// polls: a repository's live changes arrive as webhook deliveries, and its
// history is the backfill.
//
// # Configuration
//
//	# sources/github.yaml
//	id: github
//	type: github
//	containers: [acme/api]   # repositories by full name; `*` is refused
//	settings:
//	  since: 2026-01-01      # optional; backfill what changed on or after it
//	  api_url: https://ghe.example/api/v3   # optional; GitHub Enterprise Server
//	secrets:
//	  token: HEARSAY_GITHUB_TOKEN                    # required; REST calls
//	  webhook_secret: HEARSAY_GITHUB_WEBHOOK_SECRET  # required; delivery signatures
//
// The webhook is configured at the source to deliver to the runtime's hook path
// for the source (`/hooks/<source id>`), as `application/json`, with the same
// secret, for the events issues, issue_comment, pull_request,
// pull_request_review, pull_request_review_comment, push, repository and
// sub_issues.
//
// # Things to know before changing it
//
//   - A webhook delivery and a backfill page describe one object in two
//     encodings, and L0 refuses an event id that comes back with different
//     content. Every payload is therefore built from a field set both encodings
//     carry identically, normalised where they differ (a review's state is
//     upper case in REST and lower case in a webhook). The dedup test holds the
//     two to each other.
//   - No backfill step pages by offset into an ordering that can change while
//     it walks, because that skips the item at a page boundary whenever an item
//     already read moves: issues and comments are walked by updated_at, pull
//     requests in creation order, commits from a pinned head. [Connector.page]
//     says how, and the tests change the fake API's data between calls.
//   - A sub-issue's event is `part_of` its parent issue, from the
//     `parent_issue_url` both encodings carry, and a sub_issues delivery emits
//     the sub-issue again with its new parent or none. Its revision token
//     names the parent, because GitHub does not promise to move `updated_at`
//     when only the parent changes.
//   - A pull request's event carries the paths it touches, read from its files
//     list — names only, never the patch the list also carries — by the
//     backfill and again on every pull_request delivery, which does not carry
//     them. The read stops at [connector.MaxPaths] and the event says it was
//     truncated. The paths are part of the revision token, because they are
//     read separately from the pull request's `updated_at`.
//   - A re-sync ignores `since`, and a push reads its commits in one REST call
//     inside GitHub's ten-second delivery timeout; a pull_request delivery's
//     read of its files is held to the same timeout.
//   - Revision tokens compose as the contract says: the content token alone for
//     a public repository, `<content token>+perm:private` for a private one, and
//     `perm:private` alone for commits, which have no content token. The content
//     token is `updated_at` — with the parent of a sub-issue and the paths of a
//     pull request after it — except for a review, which has none and is hashed
//     from its state and body (ADR-0012). A repository going private is
//     re-synced: every artifact is emitted again under the private token and ACL.
//     `repository.privatized` records the re-sync with the runtime before the
//     delivery is answered, and the runtime walks it through [Connector.Resync],
//     keeping the position (ADR-0013). A delivery that never arrived, or
//     failed, is caught by the runtime's startup check, which asks
//     [Connector.Public] about every repository L0 still serves as public.
//
// # Reading repositories for the entity map
//
// [Reader] is not part of the connector: the assertion worker builds one per
// GitHub source a `code/` entry names, from the same source configuration, and
// seeds entities from a repository's root directories and its CODEOWNERS file
// (docs/config.md#code). It needs the token and not the webhook secret. It reads
// the repository's metadata, the root listing through the contents API — names
// and types, no content — and the files it is asked for, on the default branch,
// and keeps none of it.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Type is the connector type config selects this connector by.
const Type = "github"

// DefaultAPIURL is the REST API a source talks to unless its settings name
// another, which is how GitHub Enterprise Server is configured.
const DefaultAPIURL = "https://api.github.com"

// The secret names the connector asks config for.
const (
	// SecretToken is the credential REST calls are made with.
	SecretToken = "token"
	// SecretWebhook is the secret GitHub signs webhook deliveries with.
	SecretWebhook = "webhook_secret"
)

// requestTimeout bounds one REST call, so a hung connection is a failed
// backfill call the runtime retries rather than a backfill that never returns.
const requestTimeout = 30 * time.Second

// Settings is the connector's own configuration: the `settings` of a source.
type Settings struct {
	// Since bounds the backfill: only what changed on or after it is walked. A
	// date (`2026-01-01`) or an RFC 3339 timestamp. Empty is all of history.
	Since string `json:"since"`
	// APIURL is the REST API's base URL. Empty is [DefaultAPIURL].
	APIURL string `json:"api_url"`
}

// Connector is the GitHub connector for one configured source.
type Connector struct {
	source string
	// repos are the configured repositories, sorted, which is the order a
	// backfill walks them in.
	repos  []string
	since  time.Time
	secret []byte
	api    *client

	mu          sync.Mutex
	lastEventAt time.Time
}

// Compile-time checks that the connector implements the modes it claims.
var (
	_ connector.Pusher     = (*Connector)(nil)
	_ connector.Backfiller = (*Connector)(nil)
	_ connector.Resyncer   = (*Connector)(nil)
)

// Factory builds the connector for a source. It is what the binary registers
// under [Type].
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New builds the connector for a source, refusing a configuration it could only
// fail on later.
func New(src connector.SourceConfig) (*Connector, error) {
	sc, err := parseSource(src)
	if err != nil {
		return nil, err
	}
	secret := src.Secrets[SecretWebhook]
	if secret == "" {
		return nil, fmt.Errorf("secret %q is required: a webhook delivery that cannot be verified is refused", SecretWebhook)
	}

	return &Connector{
		source: src.ID,
		repos:  sc.repos,
		since:  sc.since,
		secret: []byte(secret),
		api:    sc.api,
	}, nil
}

// sourceConfig is what the connector and the [Reader] both take from a source:
// the repositories, the start date, and a client for the configured API with
// the token.
type sourceConfig struct {
	repos []string
	since time.Time
	api   *client
}

// parseSource checks the settings, the containers and the token. The webhook
// secret is the connector's alone, and is checked by [New].
func parseSource(src connector.SourceConfig) (sourceConfig, error) {
	var settings Settings
	if err := src.DecodeSettings(&settings); err != nil {
		return sourceConfig{}, err
	}

	repos, err := repositories(src.Containers)
	if err != nil {
		return sourceConfig{}, err
	}
	since, err := parseSince(settings.Since)
	if err != nil {
		return sourceConfig{}, err
	}
	base, err := parseAPIURL(settings.APIURL)
	if err != nil {
		return sourceConfig{}, err
	}

	for name := range src.Secrets {
		if name != SecretToken && name != SecretWebhook {
			return sourceConfig{}, fmt.Errorf("secret %q is not one the github connector reads: it reads %q and %q", name, SecretToken, SecretWebhook)
		}
	}
	token := src.Secrets[SecretToken]
	if token == "" {
		return sourceConfig{}, fmt.Errorf("secret %q is required: it is the credential REST calls are made with", SecretToken)
	}
	return sourceConfig{
		repos: repos,
		since: since,
		api:   &client{base: base, token: token, http: &http.Client{Timeout: requestTimeout}},
	}, nil
}

// repositories checks the containers are repository full names. `*` is
// refused: a backfill walks named repositories, and the connector has no way to
// honour "everything the token can see" that is not a different connector.
func repositories(containers []string) ([]string, error) {
	if len(containers) == 0 {
		return nil, errors.New("containers is empty: name the repositories to ingest, as owner/name")
	}
	repos := make([]string, 0, len(containers))
	for _, c := range containers {
		if c == connector.AllowAll {
			return nil, errors.New(`containers "*" is not supported by the github connector: name the repositories, as owner/name`)
		}
		owner, name, ok := strings.Cut(c, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("container %q is not a repository full name (owner/name)", c)
		}
		repos = append(repos, c)
	}
	slices.Sort(repos)
	return slices.Compact(repos), nil
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("settings.since %q is neither a date (2006-01-02) nor an RFC 3339 timestamp", s)
	}
	return t.UTC(), nil
}

func parseAPIURL(s string) (*url.URL, error) {
	if s == "" {
		s = DefaultAPIURL
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("settings.api_url %q is not an http(s) URL with a host and no query", s)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// Describe implements [connector.Connector].
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{
		Type: Type,
		Kinds: []connector.Kind{
			connector.KindIssue,
			connector.KindMessage,
			connector.KindPullRequest,
			connector.KindReview,
			connector.KindReviewComment,
			connector.KindCommit,
			connector.KindTombstone,
		},
	}
}

// Health implements [connector.Connector]. It makes no network call.
func (c *Connector) Health(context.Context) connector.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return connector.Health{Status: connector.HealthOK, LastEventAt: c.lastEventAt}
}

// Close implements [connector.Connector]. The connector starts no goroutine: a
// re-sync is driven by the runtime, which reports how it is going.
func (c *Connector) Close(context.Context) error { return nil }

// emit writes one event and notes when the connector last did.
func (c *Connector) emit(ctx context.Context, sink connector.Sink, ev connector.Event) error {
	if err := sink.Emit(ctx, ev); err != nil {
		return err
	}
	c.mu.Lock()
	c.lastEventAt = time.Now()
	c.mu.Unlock()
	return nil
}

// canonical is the spelling of a repository name the connector uses in ids and
// containers: the configured one where the source's name matches it but for
// case, because GitHub's names are case-insensitive and the allowlist and every
// artifact id compare bytes.
func (c *Connector) canonical(fullName string) string { return canonicalIn(c.repos, fullName) }

// view is one repository as an event is built in it, spelled the configured
// way.
func (c *Connector) view(fullName string, private bool) view {
	return view{source: c.source, repo: c.canonical(fullName), private: private, repos: c.repos}
}

func canonicalIn(repos []string, fullName string) string {
	for _, r := range repos {
		if strings.EqualFold(r, fullName) {
			return r
		}
	}
	return fullName
}

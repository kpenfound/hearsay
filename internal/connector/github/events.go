package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// GitHub's objects, as far as the connector reads them. REST and webhooks send
// the same shapes; the fields here are the ones both send identically, because
// an event built from one must equal the event built from the other.

type user struct {
	Login  string `json:"login"`
	NodeID string `json:"node_id"`
	Type   string `json:"type"`
}

type label struct {
	Name string `json:"name"`
}

type repository struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

type issue struct {
	// URL is the issue's API URL, which is how a sub-issue names its parent.
	URL       string     `json:"url"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	User      *user      `json:"user"`
	State     string     `json:"state"`
	Labels    []label    `json:"labels"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	// PullRequest is set on the issues list for an issue that is a pull
	// request, which the backfill reads from the pulls list instead.
	PullRequest json.RawMessage `json:"pull_request"`
	// ParentIssueURL is the API URL of the issue this one is a sub-issue of,
	// absent where it has no parent.
	ParentIssueURL string `json:"parent_issue_url"`
	// RepositoryURL is the API URL of the repository the issue is in, which a
	// sub_issues delivery needs: it is sent to both ends of the relationship.
	RepositoryURL string `json:"repository_url"`
}

type gitRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type pull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	User      *user      `json:"user"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	Labels    []label    `json:"labels"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
	Head      gitRef     `json:"head"`
	Base      gitRef     `json:"base"`
}

// comment is an issue comment or a review comment; the two differ in which URL
// names what they hang off.
type comment struct {
	ID             int64     `json:"id"`
	Body           string    `json:"body"`
	User           *user     `json:"user"`
	HTMLURL        string    `json:"html_url"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	IssueURL       string    `json:"issue_url"`
	PullRequestURL string    `json:"pull_request_url"`
	// The review comment fields that do not move: `line` and `commit_id` are
	// rewritten when a diff goes stale, without `updated_at` changing.
	Path             string `json:"path"`
	OriginalCommitID string `json:"original_commit_id"`
	InReplyToID      int64  `json:"in_reply_to_id"`
	ReviewID         int64  `json:"pull_request_review_id"`
}

type review struct {
	ID          int64      `json:"id"`
	User        *user      `json:"user"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	SubmittedAt *time.Time `json:"submitted_at"`
	CommitID    string     `json:"commit_id"`
}

type gitActor struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

type commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Author  *user  `json:"author"`
	Commit  struct {
		Message   string   `json:"message"`
		Author    gitActor `json:"author"`
		Committer gitActor `json:"committer"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// ghostNodeID is GitHub's own stable id for the user it shows in place of a
// deleted account, `ghost`. It stands in for an author GitHub does not name —
// a deleted user, a commit whose email belongs to no account — because every
// kind the connector emits requires an author and the contract forbids keying
// one on a login or an email.
const ghostNodeID = "MDQ6VXNlcjEwMTM3"

// permPrivate is the permission part of a revision token on an artifact in a
// private repository. GitHub has no version for a repository's visibility, so
// the connector composes one (docs/connector-contract.md, GitHub).
const permPrivate = "perm:private"

// view is one repository as an event is built in it: its canonical name and
// whether it is private, which decides the ACL and the revision tokens.
type view struct {
	source  string
	repo    string
	private bool
	// repos are the configured repositories, for spelling a repository an
	// artifact in this one points at.
	repos []string
	// readOnly is a source whose `/hearsay` comments are not commands.
	readOnly bool
}

func (v view) container() connector.Container {
	return connector.Container{Kind: connector.ContainerRepository, NativeID: v.repo, Name: v.repo}
}

// acl is `public` for a public repository and the repository's collaborators,
// as a group resolved at read time, for a private one.
func (v view) acl() connector.ACL {
	if v.private {
		return connector.ACL{{Kind: connector.ACLGroup, Source: v.source, NativeID: v.repo}}
	}
	return connector.ACL{{Kind: connector.ACLPublic}}
}

// revise returns the native id and revision for an artifact whose content token
// is content ("" for one that never changes): the token composed with the
// permission part in a private repository, and no revision at all for an
// unchanging artifact in a public one.
func (v view) revise(artifact, content string, editedAt time.Time) (string, *connector.Revision) {
	token := content
	if v.private {
		if token == "" {
			token = permPrivate
		} else {
			token += "+" + permPrivate
		}
	}
	if token == "" {
		return artifact, nil
	}
	return artifact + "@" + token, &connector.Revision{Token: token, EditedAt: editedAt}
}

func (v view) identity(u *user) *connector.Identity {
	if u == nil || u.NodeID == "" {
		return &connector.Identity{Source: v.source, Kind: connector.IdentityUser, NativeID: ghostNodeID, Handle: "ghost"}
	}
	kind := connector.IdentityUser
	if u.Type == "Bot" {
		kind = connector.IdentityBot
	}
	return &connector.Identity{Source: v.source, Kind: kind, NativeID: u.NodeID, Handle: u.Login}
}

// event is the part of every event that is the same for every kind.
func (v view) event(kind connector.Kind, artifact, content string, at, editedAt time.Time, native any) (connector.Event, error) {
	raw, err := json.Marshal(native)
	if err != nil {
		return connector.Event{}, fmt.Errorf("encoding the native fields of %s: %w", artifact, err)
	}
	nativeID, rev := v.revise(artifact, content, editedAt)
	return connector.Event{
		Source:   v.source,
		NativeID: nativeID,
		Kind:     kind,
		Time:     at.UTC(),
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: v.container(),
			Revision:  rev,
			Native:    raw,
		},
		ACL: v.acl(),
	}, nil
}

func issueArtifact(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

func commentArtifact(repo string, number int, id int64) string {
	return issueArtifact(repo, number) + ":comment:" + strconv.FormatInt(id, 10)
}

func reviewArtifact(repo string, number int, id int64) string {
	return issueArtifact(repo, number) + ":review:" + strconv.FormatInt(id, 10)
}

func commitArtifact(repo, sha string) string { return repo + "@" + sha }

// stamp is a timestamp as a revision token: GitHub's own spelling, in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func labelNames(labels []label) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

type issueNative struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	Labels   []string   `json:"labels"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
}

// issueEvent is an issue, part of the issue its parent_issue_url names. Its
// content token is its updated_at, and `<updated_at>+parent:<parent artifact>`
// for a sub-issue: GitHub does not promise to move updated_at when an issue
// changes parent, and two observations that differ in part_of must not share a
// native id (docs/connector-contract.md).
func (v view) issueEvent(is issue) (connector.Event, error) {
	artifact := issueArtifact(v.repo, is.Number)
	var parent string
	if is.ParentIssueURL != "" {
		repo, n, err := issueAt(is.ParentIssueURL)
		if err != nil {
			return connector.Event{}, fmt.Errorf("the parent of issue %s: %w", artifact, err)
		}
		parent = issueArtifact(canonicalIn(v.repos, repo), n)
	}
	token := stamp(is.UpdatedAt)
	if parent != "" {
		token += "+parent:" + parent
	}
	ev, err := v.event(connector.KindIssue, artifact, token, is.CreatedAt, is.UpdatedAt.UTC(), issueNative{
		Number: is.Number, State: is.State, Labels: labelNames(is.Labels), ClosedAt: utc(is.ClosedAt),
	})
	if err != nil {
		return ev, err
	}
	ev.Payload.URL = is.HTMLURL
	ev.Payload.Title = is.Title
	ev.Payload.Text = is.Body
	ev.Payload.Author = v.identity(is.User)
	ev.Payload.PartOf = parent
	return ev, nil
}

// issueAt is the repository and number of the issue an API URL names:
// `.../repos/acme/api/issues/12`.
func issueAt(apiURL string) (string, int, error) {
	i := strings.Index(apiURL, "/repos/")
	if i < 0 {
		return "", 0, fmt.Errorf("%q does not name an issue (no /repos/)", apiURL)
	}
	repo, number, ok := strings.Cut(apiURL[i+len("/repos/"):], "/issues/")
	owner, name, _ := strings.Cut(repo, "/")
	n, err := strconv.Atoi(number)
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%q does not name an issue", apiURL)
	}
	return repo, n, nil
}

// repositoryAt is the full name of the repository an API URL names:
// `.../repos/acme/api`.
func repositoryAt(apiURL string) (string, bool) {
	i := strings.Index(apiURL, "/repos/")
	if i < 0 {
		return "", false
	}
	repo := apiURL[i+len("/repos/"):]
	owner, name, _ := strings.Cut(repo, "/")
	return repo, owner != "" && name != "" && !strings.Contains(name, "/")
}

type pullNative struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	Draft    bool       `json:"draft"`
	Labels   []string   `json:"labels"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	Head     gitRef     `json:"head"`
	Base     gitRef     `json:"base"`
}

// changedFile is one entry of a pull request's files list, as far as the
// connector reads it: the names. The entry also carries the patch, which is
// never decoded.
type changedFile struct {
	Filename string `json:"filename"`
	// PreviousFilename is the name a renamed file had.
	PreviousFilename string `json:"previous_filename"`
}

// touched is what a pull request's files say it touched, in the shape
// payload.paths takes: every name, and every name a renamed file had.
type touched struct {
	paths     []string
	truncated bool
}

// pathsToken is the part of a pull request's content token that names what it
// touches: `paths:` and the first 16 hex digits of the SHA-256 of every path
// followed by a newline, then `truncated` where the list was cut. A path has
// no control character in it (connector.BoundPaths), so the encoding is
// unambiguous. It is empty for a pull request that touches nothing.
func pathsToken(t touched) string {
	if len(t.paths) == 0 {
		return ""
	}
	h := sha256.New()
	for _, p := range t.paths {
		h.Write([]byte(p + "\n"))
	}
	if t.truncated {
		h.Write([]byte("truncated"))
	}
	return "paths:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// pullEvent is a pull request and the paths it touches. Its content token is
// its updated_at, and `<updated_at>+paths:<hash>` where it touches any: the
// files list is read separately from the pull request, and two observations
// that differ in it must not share a native id (docs/connector-contract.md).
func (v view) pullEvent(p pull, t touched) (connector.Event, error) {
	artifact := issueArtifact(v.repo, p.Number)
	token := stamp(p.UpdatedAt)
	if pt := pathsToken(t); pt != "" {
		token += "+" + pt
	}
	ev, err := v.event(connector.KindPullRequest, artifact, token, p.CreatedAt, p.UpdatedAt.UTC(), pullNative{
		Number: p.Number, State: p.State, Draft: p.Draft, Labels: labelNames(p.Labels),
		ClosedAt: utc(p.ClosedAt), MergedAt: utc(p.MergedAt), Head: p.Head, Base: p.Base,
	})
	if err != nil {
		return ev, err
	}
	ev.Payload.URL = p.HTMLURL
	ev.Payload.Title = p.Title
	ev.Payload.Text = p.Body
	ev.Payload.Author = v.identity(p.User)
	ev.Payload.Paths, ev.Payload.PathsTruncated = t.paths, t.truncated
	return ev, nil
}

// numberFrom is the issue or pull request number at the end of an API URL:
// `.../issues/12`, `.../pulls/31`.
func numberFrom(apiURL, segment string) (int, error) {
	i := strings.LastIndex(apiURL, segment)
	if i < 0 {
		return 0, fmt.Errorf("%q does not name what it hangs off (no %s)", apiURL, segment)
	}
	n, err := strconv.Atoi(apiURL[i+len(segment):])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q does not end in a number after %s", apiURL, segment)
	}
	return n, nil
}

// commentOn is the artifact an issue comment is, and the issue or pull request
// it hangs off.
func (v view) commentOn(cm comment) (artifact, parent string, err error) {
	n, err := numberFrom(cm.IssueURL, "/issues/")
	if err != nil {
		return "", "", fmt.Errorf("issue comment %d: %w", cm.ID, err)
	}
	return commentArtifact(v.repo, n, cm.ID), issueArtifact(v.repo, n), nil
}

// reviewCommentOn is the same for a review comment.
func (v view) reviewCommentOn(cm comment) (artifact, parent string, err error) {
	n, err := numberFrom(cm.PullRequestURL, "/pulls/")
	if err != nil {
		return "", "", fmt.Errorf("review comment %d: %w", cm.ID, err)
	}
	return commentArtifact(v.repo, n, cm.ID), issueArtifact(v.repo, n), nil
}

type commentNative struct {
	ID int64 `json:"id"`
}

// commentEvent is an issue or pull request comment: a `message`, or, where it
// is a `/hearsay` command or Hearsay's reply to one, a `command` or a
// [KindReply] ([controlEvent]).
func (v view) commentEvent(cm comment) (connector.Event, error) {
	artifact, parent, err := v.commentOn(cm)
	if err != nil {
		return connector.Event{}, err
	}
	ev, err := v.event(connector.KindMessage, artifact, stamp(cm.UpdatedAt), cm.CreatedAt, cm.UpdatedAt.UTC(), commentNative{ID: cm.ID})
	if err != nil {
		return ev, err
	}
	ev.Payload.URL = cm.HTMLURL
	ev.Payload.Text = cm.Body
	ev.Payload.Author = v.identity(cm.User)
	ev.Payload.Parent, ev.Payload.Thread = parent, parent
	n, _ := numberFrom(cm.IssueURL, "/issues/")
	return controlEvent(ev, cm, n, v.repo, v.readOnly)
}

type reviewCommentNative struct {
	ID               int64  `json:"id"`
	Path             string `json:"path,omitempty"`
	OriginalCommitID string `json:"original_commit_id,omitempty"`
	InReplyToID      int64  `json:"in_reply_to_id,omitempty"`
	ReviewID         int64  `json:"review_id,omitempty"`
}

func (v view) reviewCommentEvent(cm comment) (connector.Event, error) {
	artifact, parent, err := v.reviewCommentOn(cm)
	if err != nil {
		return connector.Event{}, err
	}
	ev, err := v.event(connector.KindReviewComment, artifact, stamp(cm.UpdatedAt), cm.CreatedAt, cm.UpdatedAt.UTC(), reviewCommentNative{
		ID: cm.ID, Path: cm.Path, OriginalCommitID: cm.OriginalCommitID, InReplyToID: cm.InReplyToID, ReviewID: cm.ReviewID,
	})
	if err != nil {
		return ev, err
	}
	ev.Payload.URL = cm.HTMLURL
	ev.Payload.Text = cm.Body
	ev.Payload.Author = v.identity(cm.User)
	ev.Payload.Parent, ev.Payload.Thread = parent, parent
	return ev, nil
}

type reviewNative struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	CommitID string `json:"commit_id,omitempty"`
}

// submitted reports whether a review has been submitted: a pending review is a
// draft only its author can see, and is not an artifact yet.
func (r review) submitted() bool {
	return r.SubmittedAt != nil && !strings.EqualFold(r.State, "pending")
}

// reviewToken is a review's content token: the first 16 hex digits of the
// SHA-256 of its lower-case state, a newline, and its body (ADR-0012). GitHub
// moves no version field when a review is edited or dismissed, so what changed
// is hashed instead; a state has no newline in it, so the two cannot run
// together.
func reviewToken(r review) string {
	sum := sha256.Sum256([]byte(strings.ToLower(r.State) + "\n" + r.Body))
	return hex.EncodeToString(sum[:])[:16]
}

// reviewEvent is a submitted review on pull request number. Its state is lower
// case whichever encoding it came in: REST says `APPROVED`, a webhook
// `approved`. GitHub gives no time for an edit or a dismissal, so the revision
// has no edited_at and ingest order decides which is current.
func (v view) reviewEvent(number int, r review) (connector.Event, error) {
	artifact := reviewArtifact(v.repo, number, r.ID)
	parent := issueArtifact(v.repo, number)
	ev, err := v.event(connector.KindReview, artifact, reviewToken(r), *r.SubmittedAt, time.Time{}, reviewNative{
		ID: r.ID, State: strings.ToLower(r.State), CommitID: r.CommitID,
	})
	if err != nil {
		return ev, err
	}
	ev.Payload.URL = r.HTMLURL
	ev.Payload.Text = r.Body
	ev.Payload.Author = v.identity(r.User)
	ev.Payload.Parent, ev.Payload.Thread = parent, parent
	return ev, nil
}

type commitNative struct {
	SHA        string    `json:"sha"`
	Parents    []string  `json:"parents"`
	AuthoredAt time.Time `json:"authored_at"`
}

// commitEvent is a commit on the default branch, timed when it was committed.
// Its author is the GitHub account the commit is attributed to, with the git
// author's name and email as hints; a commit attributed to no account has the
// ghost's id and the same hints.
func (v view) commitEvent(c commit) (connector.Event, error) {
	parents := make([]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, p.SHA)
	}
	ev, err := v.event(connector.KindCommit, commitArtifact(v.repo, c.SHA), "", c.Commit.Committer.Date, time.Time{}, commitNative{
		SHA: c.SHA, Parents: parents, AuthoredAt: c.Commit.Author.Date.UTC(),
	})
	if err != nil {
		return ev, err
	}
	title, _, _ := strings.Cut(c.Commit.Message, "\n")
	ev.Payload.URL = c.HTMLURL
	ev.Payload.Title = strings.TrimSpace(title)
	ev.Payload.Text = c.Commit.Message
	author := v.identity(c.Author)
	author.DisplayName = c.Commit.Author.Name
	author.Email = c.Commit.Author.Email
	ev.Payload.Author = author
	return ev, nil
}

// tombstone retracts target, timed by the deleted object's own last update:
// GitHub sends no time for a deletion, and a tombstone timed by when the
// delivery arrived would be a different event on every redelivery.
func (v view) tombstone(target string, at time.Time) (connector.Event, error) {
	ev, err := v.event(connector.KindTombstone, target+":tombstone", "", at, time.Time{}, struct{}{})
	if err != nil {
		return ev, err
	}
	ev.Payload.Native = nil
	ev.Payload.Target = target
	return ev, nil
}

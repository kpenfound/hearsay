// Package osmia reads committed Osmia traces through a read-only local mount.
package osmia

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Type is the connector registry key.
const Type = "osmia"
const pageSize = 100

var projectID = regexp.MustCompile(`^p_[0-9a-f]{32}$`)
var workstreamID = regexp.MustCompile(`^w_[0-9a-f]{32}$`)
var objectID = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// Settings selects a private, explicitly allowlisted part of the mounted trace.
// Owner and work channels use different source IDs so authority can distinguish
// human decisions from drafts and agent contributions.
type Settings struct {
	Root              string             `json:"root"`
	Owner             connector.Identity `json:"owner"`
	Channel           string             `json:"channel"`
	PermissionVersion string             `json:"permission_version"`
}

// Connector pages committed records without acquiring the Osmia writer lock.
type Connector struct {
	source     string
	settings   Settings
	projects   []string
	version    string
	permission string
	mu         sync.Mutex
	health     connector.Health
	cursor     connector.Cursor
}

type position struct {
	Version string `json:"version"`
	Project int    `json:"project"`
	Commit  string `json:"commit,omitempty"`
	File    int    `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

var _ connector.CursorPoller = (*Connector)(nil)
var _ connector.BackfillVersioner = (*Connector)(nil)

// Factory constructs the configured trace reader.
func Factory(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
	return New(src)
}

// New validates the mount, private owner and explicit project allowlist.
func New(src connector.SourceConfig) (*Connector, error) {
	var s Settings
	if err := src.DecodeSettings(&s); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(s.Root) || (s.Channel != "owner" && s.Channel != "work") {
		return nil, errors.New("osmia requires an absolute root and channel owner or work")
	}
	if !connector.ValidSourceID(src.ID) || !connector.ValidSourceID(s.Owner.Source) || s.Owner.Kind != connector.IdentityUser || s.Owner.NativeID == "" {
		return nil, errors.New("osmia requires a source ID and an owner user identity")
	}
	if len(src.Secrets) != 0 || len(src.Containers) == 0 {
		return nil, errors.New("osmia requires explicit project containers and no secrets")
	}
	projects := slices.Clone(src.Containers)
	slices.Sort(projects)
	for i, p := range projects {
		if !projectID.MatchString(p) || i > 0 && projects[i-1] == p {
			return nil, errors.New("osmia containers must be distinct project IDs")
		}
	}
	if err := realDirectory(s.Root); err != nil {
		return nil, err
	}
	permission, _ := json.Marshal(struct {
		Owner   connector.Identity
		Version string
	}{s.Owner, s.PermissionVersion})
	conf, _ := json.Marshal(struct {
		Settings Settings
		Projects []string
	}{s, projects})
	c := &Connector{source: src.ID, settings: s, projects: projects, permission: fmt.Sprintf("%x", sha256.Sum256(permission)), version: fmt.Sprintf("osmia-v1-%x", sha256.Sum256(conf)), health: connector.Health{Status: connector.HealthOK}}
	return c, nil
}

func realDirectory(dir string) error {
	// Refuse symlinks at every component, including the mounted root's parents.
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return errors.New("osmia trace directory is unavailable")
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("osmia trace mount must contain real directories")
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// Describe declares the L0 event kinds this reader emits.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindDocument, connector.KindTombstone, connector.KindThread, connector.KindMessage, connector.KindAgentSession, connector.KindAgentTurn, connector.KindToolCall, connector.KindCommit, "osmia.transition", "osmia.ruling"}}
}

// Health reports the latest ingestion result without reading the source.
func (c *Connector) Health(context.Context) connector.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}

// Close releases this stateless reader.
func (c *Connector) Close(context.Context) error { return nil }

// BackfillVersion identifies the source inputs and permission generation.
func (c *Connector) BackfillVersion() string { return c.version }

// Poll advances an in-memory cursor for runtimes without durable polling.
func (c *Connector) Poll(ctx context.Context, sink connector.Sink) error {
	next, err := c.PollFrom(ctx, sink, c.cursor)
	if err == nil {
		c.cursor = next
	}
	return err
}

// PollFrom advances the runtime-owned polling cursor.
func (c *Connector) PollFrom(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.Cursor, error) {
	if from == "" {
		if err := c.retractRemoved(ctx, sink); err != nil {
			return from, err
		}
	}
	result, err := c.Backfill(ctx, sink, from)
	if err != nil {
		return from, err
	}
	if result.Done {
		return "", nil
	}
	return result.Next, nil
}

// Backfill emits one bounded page from an immutable trace commit.
func (c *Connector) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (result connector.BackfillResult, err error) {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.health = connector.Health{Status: connector.HealthDegraded, Detail: "committed trace ingestion failed; check mount and source configuration"}
		} else {
			c.health.Status = connector.HealthOK
			c.health.Detail = ""
			if result.Events > 0 {
				c.health.LastEventAt = time.Now().UTC()
			}
		}
	}()
	pos := position{Version: c.version}
	if from != "" {
		if err = json.Unmarshal([]byte(from), &pos); err != nil {
			return result, errors.New("invalid osmia cursor")
		}
		if pos.Version != c.version {
			pos = position{Version: c.version}
		}
	}
	if pos.Project < 0 || pos.Project > len(c.projects) || pos.File < 0 || pos.Line < 0 || pos.Commit != "" && !objectID.MatchString(pos.Commit) {
		return result, errors.New("invalid osmia cursor position")
	}
	for pos.Project < len(c.projects) {
		project := c.projects[pos.Project]
		dir := filepath.Join(c.settings.Root, "projects", project)
		if err = realDirectory(dir); err != nil {
			return result, err
		}
		if err = realDirectory(filepath.Join(dir, ".git")); err != nil {
			return result, err
		}
		if pos.Commit == "" {
			var head []byte
			head, err = gitOutput(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
			if err != nil {
				return result, err
			}
			pos.Commit = strings.TrimSpace(string(head))
			if !objectID.MatchString(pos.Commit) {
				return result, errors.New("invalid trace commit")
			}
		}
		var tree []byte
		tree, err = gitOutput(ctx, dir, "ls-tree", "-r", "-z", pos.Commit)
		if err != nil {
			return result, err
		}
		var files []string
		for _, entry := range strings.Split(string(tree), "\x00") {
			meta, name, ok := strings.Cut(entry, "\t")
			if ok && strings.HasPrefix(meta, "100644 blob ") && recordFile(name) {
				files = append(files, name)
			}
		}
		slices.Sort(files)
		for pos.File < len(files) {
			var next int
			var done bool
			var count int
			next, done, count, err = c.read(ctx, sink, dir, project, pos.Commit, files[pos.File], pos.Line, pageSize-result.Events)
			if err != nil {
				return result, err
			}
			result.Events += count
			pos.Line = next
			if done {
				pos.File++
				pos.Line = 0
			}
			if !done || result.Events >= pageSize {
				data, _ := json.Marshal(pos)
				result.Next = connector.Cursor(data)
				return result, nil
			}
		}
		pos.Project++
		pos.Commit = ""
		pos.File = 0
		pos.Line = 0
	}
	result.Done = true
	return result, nil
}

func recordFile(name string) bool {
	if name == "documents.jsonl" {
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) < 3 || parts[0] != "workstreams" || !workstreamID.MatchString(parts[1]) {
		return false
	}
	switch {
	case len(parts) == 3:
		return parts[2] == "documents.jsonl" || parts[2] == "events.jsonl"
	case len(parts) == 5 && parts[2] == "agents":
		return parts[4] == "identity.jsonl" || parts[4] == "log.jsonl"
	case len(parts) == 5 && parts[2] == "questions":
		return parts[4] == "question.jsonl" || parts[4] == "rulings.jsonl"
	case len(parts) == 5 && parts[2] == "amendments":
		return parts[4] == "request.jsonl"
	}
	return false
}

func command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=" + dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
	return cmd
}
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := command(ctx, dir, args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, 8<<20+1))
	if len(data) > 8<<20 {
		_ = cmd.Process.Kill()
		readErr = errors.New("trace tree exceeds 8 MiB")
	}
	waitErr := cmd.Wait()
	return data, errors.Join(readErr, waitErr)
}

func (c *Connector) read(ctx context.Context, sink connector.Sink, dir, project, commit, file string, start, limit int) (next int, done bool, count int, err error) {
	cmd := command(ctx, dir, "show", commit+":"+file)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return 0, false, 0, err
	}
	if err = cmd.Start(); err != nil {
		return 0, false, 0, err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scan := bufio.NewScanner(pipe)
	scan.Buffer(make([]byte, 4096), 4<<20)
	created := map[string]time.Time{}
	for scan.Scan() {
		var r record
		if err = json.Unmarshal(scan.Bytes(), &r); err != nil {
			return next, false, count, errors.New("malformed committed trace record")
		}
		identity := r.Schema + "/" + r.ID
		if _, ok := created[identity]; !ok {
			created[identity] = r.At
		}
		next++
		if next <= start {
			continue
		}
		var events []connector.Event
		events, err = c.events(project, r, created[identity])
		if err != nil {
			return next, false, count, err
		}
		// All effects of a record share the cursor boundary.
		for _, event := range events {
			if err = sink.Emit(ctx, event); err != nil {
				return next, false, count, err
			}
			count++
		}
		if next-start >= limit {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			stopped = true
			return next, false, count, nil
		}
	}
	if err = scan.Err(); err != nil {
		return next, false, count, err
	}
	err = cmd.Wait()
	stopped = true
	return next, true, count, err
}

type record struct {
	Schema     string    `json:"schema"`
	Version    int       `json:"version"`
	ID         string    `json:"id"`
	Revision   int       `json:"revision"`
	Project    string    `json:"project"`
	Workstream string    `json:"workstream"`
	Unit       string    `json:"unit"`
	At         time.Time `json:"at"`
	Actor      struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"actor"`
	Cause           string          `json:"cause"`
	Path            string          `json:"path"`
	Content         string          `json:"content"`
	Question        string          `json:"question"`
	QuestionID      string          `json:"question_id"`
	Decision        string          `json:"decision"`
	OwnerResponse   string          `json:"owner_response"`
	ReturnedAnswer  string          `json:"returned_answer"`
	Subject         string          `json:"subject"`
	To              string          `json:"to"`
	Reason          string          `json:"reason"`
	Prompt          string          `json:"prompt"`
	ThreadID        string          `json:"thread_id"`
	TurnID          string          `json:"turn_id"`
	AgentID         string          `json:"agent_id"`
	Role            string          `json:"role"`
	Session         json.RawMessage `json:"session"`
	RequestID       string          `json:"request_id"`
	RequestRevision int             `json:"request_revision"`
	Result          json.RawMessage `json:"result"`
}

func (c *Connector) events(project string, r record, created time.Time) ([]connector.Event, error) {
	if r.Project != project || (r.Workstream != "" && !workstreamID.MatchString(r.Workstream)) || r.Version != 1 || r.ID == "" || r.Revision < 1 || r.At.IsZero() {
		return nil, errors.New("unsupported trace record identity or version")
	}
	kind := connector.KindMessage
	var base connector.Kind
	text := r.Content
	owner := false
	switch r.Schema {
	case "osmia.trace.document":
		switch {
		case strings.HasPrefix(r.Path, "memory/"), strings.HasSuffix(r.Path, "notes.md"), strings.HasPrefix(r.Path, "inspections/"):
			return nil, nil
		case r.Path == "charter.md":
			kind = connector.KindDocument
			owner = true
		case strings.HasPrefix(r.Path, "tools/"):
			kind = connector.KindToolCall
		case r.Path == "spec.md", r.Path == "plan.json", strings.HasPrefix(r.Path, "kb/") && strings.HasSuffix(r.Path, ".md"):
			kind = connector.KindDocument
		case strings.HasSuffix(r.Path, "/landing.json"):
			kind = connector.KindCommit
		case r.Path == "seal.json":
			kind = "osmia.transition"
			base = connector.KindMessage
		case strings.HasPrefix(r.Path, "shed/") && strings.HasSuffix(r.Path, "/packet.json"):
			kind = connector.KindThread
		case strings.HasPrefix(r.Path, "shed/"), strings.HasPrefix(r.Path, "units/"), strings.HasPrefix(r.Path, "messages/"), strings.HasPrefix(r.Path, "notices/"), strings.HasPrefix(r.Path, "handed/"):
		default:
			return nil, nil
		}
	case "osmia.trace.ruling":
		kind = "osmia.ruling"
		base = connector.KindMessage
		text = r.ReturnedAnswer
		if r.Decision == "ruling" && r.Revision != 1 {
			return nil, nil
		}
		if r.Decision == "ruling" && r.OwnerResponse != "" && r.Actor.Kind == "owner" {
			owner = true
			base = connector.KindDocument
			text = r.OwnerResponse
		}
	case "osmia.trace.question":
		kind = connector.KindThread
		text = r.Question
	case "osmia.trace.transition":
		kind = "osmia.transition"
		base = connector.KindMessage
		text = r.Reason
		if r.To == "delivered" {
			kind = connector.KindCommit
			base = ""
		}
	case "osmia.trace.turn-request":
		kind = connector.KindAgentSession
		text = "Turn started: " + r.TurnID + "\n" + r.Prompt
	case "osmia.trace.turn-response":
		kind = connector.KindAgentTurn
		text = string(r.Result)
	case "osmia.trace.agent":
		kind = connector.KindAgentSession
		text = "Durable agent thread: " + r.ThreadID
	case "osmia.trace.amendment":
		kind = connector.KindMessage
	default:
		return nil, nil
	}
	if owner != (c.settings.Channel == "owner") {
		return nil, nil
	}
	artifact := project + "/" + r.Workstream + "/" + strings.TrimPrefix(r.Schema, "osmia.trace.") + "/" + r.ID
	author := connector.Identity{Source: c.source, Kind: connector.IdentityAgent, NativeID: r.Actor.ID}
	if author.NativeID == "" {
		author.NativeID = "service"
	}
	if r.Actor.Kind == "agent" {
		switch r.Actor.ID {
		case "chief_of_staff":
			author.NativeID = "orchestrator"
		case "librarian", "agent_librarian", "foreman":
			author.NativeID = "observer"
		default:
			author.NativeID = "worker"
		}
	}
	if owner || r.Actor.Kind == "owner" {
		author = c.settings.Owner
	} else if r.Actor.Kind == "service" {
		author.Kind = connector.IdentityBot
	}
	token := strconv.Itoa(r.Revision) + "-" + c.permission[:16]
	payload := connector.Payload{Artifact: artifact, BaseKind: base, Container: connector.Container{Kind: connector.ContainerWorkspace, NativeID: project}, Title: r.ID, Text: text, Author: &author, Revision: &connector.Revision{Token: token, EditedAt: r.At}}
	// Preserve only record provenance and relevant content, never arbitrary config,
	// private role notes, runtime credentials or embedded copies of memory bundles.
	payload.Native, _ = json.Marshal(map[string]any{"schema": r.Schema, "id": r.ID, "revision": r.Revision, "project": project, "workstream": r.Workstream, "unit": r.Unit, "actor": r.Actor, "cause": r.Cause, "path": r.Path, "turn": r.TurnID, "thread": r.ThreadID, "subject": r.Subject, "state": r.To, "question": r.QuestionID, "agent": r.AgentID, "role": r.Role, "session": r.Session, "request": r.RequestID, "request_revision": r.RequestRevision})
	if kind == connector.KindMessage {
		if r.QuestionID != "" {
			payload.Thread = project + "/" + r.Workstream + "/question/" + r.QuestionID
		}
		if strings.HasPrefix(r.Path, "shed/round-") {
			parts := strings.Split(r.Path, "/")
			if len(parts) == 3 {
				payload.Thread = project + "/" + r.Workstream + "/document/shed-" + parts[1] + "-packet"
			}
		}
	}
	if kind == connector.KindDocument && text == "" {
		payload.Target = artifact
		artifact += ":deleted:" + strconv.Itoa(r.Revision)
		payload.Artifact = artifact
		kind = connector.KindTombstone
		created = r.At
	}
	if created.After(r.At) {
		payload.Revision.EditedAt = created
	}
	event := connector.Event{Source: c.source, NativeID: artifact + "@" + token, Kind: kind, Time: created, Payload: payload, ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: c.settings.Owner.Source, NativeID: c.settings.Owner.NativeID}}}
	event.ID = connector.EventID(event.Source, event.NativeID)
	if err := event.Validate(); err != nil {
		return nil, err
	}
	events := []connector.Event{event}
	if r.Schema == "osmia.trace.turn-response" {
		end := event
		end.Kind = connector.KindAgentSession
		end.Payload.Artifact += "/session-end"
		end.NativeID = end.Payload.Artifact + "@" + token
		end.ID = connector.EventID(end.Source, end.NativeID)
		end.Payload.Text = "Turn ended: " + r.TurnID
		events = append(events, end)
	}
	return events, nil
}

// retractRemoved closes artifacts from containers that left the explicit
// allowlist. Historical L0 observations remain available as provenance.
func (c *Connector) retractRemoved(ctx context.Context, sink connector.Sink) error {
	inventory, ok := sink.(connector.ArtifactReader)
	if !ok {
		return connector.ErrNoDocumentInventory
	}
	retractor, ok := sink.(connector.RetractionSink)
	if !ok {
		return errors.New("osmia polling requires a retraction sink")
	}
	stored, err := inventory.CurrentArtifacts(ctx, c.source)
	if err != nil {
		return err
	}
	for _, prior := range stored {
		if slices.Contains(c.projects, prior.Payload.Container.NativeID) {
			continue
		}
		token := c.version
		artifact := prior.Payload.Artifact + ":removed"
		at := prior.Time
		if prior.Payload.Revision != nil && prior.Payload.Revision.EditedAt.After(at) {
			at = prior.Payload.Revision.EditedAt
		}
		at = at.Add(time.Nanosecond)
		ev := connector.Event{Source: c.source, NativeID: artifact + "@" + token, Kind: connector.KindTombstone, Time: at, ACL: prior.ACL, Payload: connector.Payload{Artifact: artifact, Target: prior.Payload.Artifact, Container: prior.Payload.Container, Revision: &connector.Revision{Token: token, EditedAt: at}}}
		ev.ID = connector.EventID(ev.Source, ev.NativeID)
		if err := ev.Validate(); err != nil {
			return err
		}
		if err := retractor.Retract(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

package osmia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

const project = "p_0123456789abcdef0123456789abcdef"
const stream = "w_0123456789abcdef0123456789abcdef"

var at = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

type capture struct {
	events []connector.Event
	failAt int
}

func (s *capture) Emit(_ context.Context, e connector.Event) error {
	if s.failAt > 0 && len(s.events) == s.failAt {
		return errors.New("sink interrupted")
	}
	if err := e.Validate(); err != nil {
		return err
	}
	s.events = append(s.events, e)
	return nil
}
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := command(t.Context(), dir, args...)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func setup(t *testing.T, channel string) (*Connector, string, connector.SourceConfig) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "projects", project)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	settings, _ := json.Marshal(Settings{Root: root, Channel: channel, Owner: connector.Identity{Source: "people", Kind: connector.IdentityUser, NativeID: "owner"}})
	src := connector.SourceConfig{ID: "osmia-" + channel, Type: Type, Containers: []string{project}, Settings: settings}
	c, err := New(src)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir, src
}
func doc(id, path, text string, revision int) record {
	r := record{Schema: "osmia.trace.document", Version: 1, ID: id, Revision: revision, Project: project, Workstream: stream, At: at.Add(time.Duration(revision-1) * time.Second), Path: path, Content: text}
	r.Actor.Kind, r.Actor.ID = "agent", "architect"
	return r
}
func write(t *testing.T, dir, name string, records []record) {
	t.Helper()
	var lines []byte
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, append(b, '\n')...)
	}
	file := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, lines, 0600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "--all")
	git(t, dir, "commit", "-qm", "Trace records")
}
func drain(t *testing.T, c *Connector, sink connector.Sink, cursor connector.Cursor) {
	t.Helper()
	for i := 0; i < 20; i++ {
		page, err := c.Backfill(t.Context(), sink, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if page.Done {
			return
		}
		cursor = page.Next
	}
	t.Fatal("backfill failed to finish")
}
func TestBackfillPinsCommitAndReplaysAfterSinkFailure(t *testing.T) {
	c, dir, src := setup(t, "work")
	name := "workstreams/" + stream + "/documents.jsonl"
	records := []record{}
	for i := 0; i < 205; i++ {
		records = append(records, doc(fmt.Sprintf("spec-%d", i), "spec.md", "Original", 1))
	}
	write(t, dir, name, records)
	first := &capture{}
	page, err := c.Backfill(t.Context(), first, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Done || len(first.events) != 100 {
		t.Fatalf("page %+v events %d", page, len(first.events))
	}
	// The remaining page belongs to the pinned commit, even after another write.
	records = append(records, doc("spec-later", "spec.md", "Later", 1))
	write(t, dir, name, records)
	resumed, err := New(src)
	if err != nil {
		t.Fatal(err)
	}
	failed := &capture{failAt: 3}
	if _, err = resumed.Backfill(t.Context(), failed, page.Next); err == nil {
		t.Fatal("expected interrupted sink")
	}
	tail := &capture{}
	drain(t, resumed, tail, page.Next)
	if len(tail.events) != 105 {
		t.Fatalf("unpinned page %d", len(tail.events))
	}
	if !reflect.DeepEqual(failed.events, tail.events[:3]) {
		t.Fatal("unstable replay IDs or payload")
	}
	all := &capture{}
	drain(t, resumed, all, "")
	if len(all.events) != 206 {
		t.Fatalf("new snapshot %d", len(all.events))
	}
	// Dirty files and unrelated JSONL never enter ingestion.
	if err := os.WriteFile(filepath.Join(dir, name), []byte("uncommitted invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	again := &capture{}
	drain(t, resumed, again, "")
	if !reflect.DeepEqual(all.events, again.events) {
		t.Fatal("working tree influenced trace ingestion")
	}
}
func TestOwnerAuthorityRevisionsDeletionsAndPrivateNotes(t *testing.T) {
	c, dir, src := setup(t, "owner")
	charter := doc("charter", "charter.md", "1. Owner rule", 1)
	charter.Workstream = ""
	charter.Actor.Kind = "owner"
	revised := charter
	revised.Revision = 2
	revised.Content = "1. Amended rule"
	revised.At = at.Add(time.Minute)
	ruling := record{Schema: "osmia.trace.ruling", Version: 1, ID: "ruling-1", Revision: 1, Project: project, Workstream: stream, At: at, QuestionID: "question-1", Decision: "ruling", OwnerResponse: "Keep the API stable"}
	ruling.Actor.Kind = "owner"
	relay := ruling
	relay.Revision = 2
	relay.ReturnedAnswer = "Agent paraphrase"
	relay.Actor.Kind = "agent"
	write(t, dir, "documents.jsonl", []record{charter, revised})
	write(t, dir, "workstreams/"+stream+"/questions/question-1/rulings.jsonl", []record{ruling, relay})
	agent := doc("spec", "spec.md", "Draft", 1)
	deleted := agent
	deleted.Revision = 2
	deleted.Content = ""
	deleted.At = at.Add(time.Second)
	write(t, dir, "workstreams/"+stream+"/documents.jsonl", []record{agent, deleted, doc("notes", "agents/a/notes.md", "private craft", 1), doc("memory", "memory/watch-a.json", "external memory", 1)})
	sink := &capture{}
	drain(t, c, sink, "")
	if len(sink.events) != 3 {
		t.Fatalf("owner events %+v", sink.events)
	}
	for _, e := range sink.events {
		if e.Payload.Author.Kind != connector.IdentityUser || e.ACL[0].Kind != connector.ACLIdentity {
			t.Fatal("owner attribution or ACL changed")
		}
		if e.Kind == "osmia.ruling" && (e.Payload.Text != ruling.OwnerResponse || e.Payload.BaseKind != connector.KindDocument) {
			t.Fatal("owner ruling altered")
		}
	}
	if sink.events[0].Time != sink.events[1].Time || sink.events[0].ID == sink.events[1].ID {
		t.Fatal("revision identity or artifact time")
	}
	var settings Settings
	if err := json.Unmarshal(src.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	settings.Channel = "work"
	src.ID = "osmia-work"
	src.Settings, _ = json.Marshal(settings)
	work, err := New(src)
	if err != nil {
		t.Fatal(err)
	}
	proposals := &capture{}
	drain(t, work, proposals, "")
	if len(proposals.events) != 2 {
		t.Fatalf("work events %+v", proposals.events)
	}
	if proposals.events[1].Kind != connector.KindTombstone || proposals.events[1].Payload.Target != proposals.events[0].Payload.Artifact {
		t.Fatal("missing deletion")
	}
}
func TestTraceAllowlistAndSymlinksAreRefused(t *testing.T) {
	_, dir, src := setup(t, "work")
	src.Containers = []string{"*"}
	if _, err := New(src); err == nil {
		t.Fatal("wildcard accepted")
	}
	src.Containers = []string{project}
	c, err := New(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Backfill(t.Context(), &capture{}, ""); err == nil {
		t.Fatal("followed trace symlink")
	}
	if c.Health(t.Context()).Status != connector.HealthDegraded {
		t.Fatal("failed source reported healthy")
	}
}

func (s *capture) CurrentArtifacts(_ context.Context, source string) ([]connector.Event, error) {
	return slices.DeleteFunc(slices.Clone(s.events), func(e connector.Event) bool { return e.Source != source || e.Kind == connector.KindTombstone }), nil
}
func (s *capture) Retract(ctx context.Context, e connector.Event) error { return s.Emit(ctx, e) }
func TestPollingRetractsRemovedProject(t *testing.T) {
	c, _, _ := setup(t, "work")
	r := doc("spec", "spec.md", "Draft", 1)
	old, err := c.events(project, r, at)
	if err != nil {
		t.Fatal(err)
	}
	sink := &capture{events: old}
	c.projects = []string{"p_ffffffffffffffffffffffffffffffff"}
	if err := c.retractRemoved(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 || sink.events[1].Payload.Target != old[0].Payload.Artifact {
		t.Fatal("removed project remains current")
	}
}

func TestTurnAndToolProvenanceRemainAuditEvents(t *testing.T) {
	c, _, _ := setup(t, "work")
	request := record{Schema: "osmia.trace.turn-request", Version: 1, ID: "request-1", Revision: 1, Project: project, Workstream: stream, At: at, AgentID: "agent_architect", ThreadID: "architect", TurnID: "draft-1", Prompt: "Draft with scoped context"}
	request.Actor.Kind, request.Actor.ID = "agent", "agent_architect"
	response := request
	response.Schema = "osmia.trace.turn-response"
	response.ID = "response-1"
	response.RequestID = request.ID
	response.RequestRevision = 1
	response.Result = json.RawMessage(`{"session":{"backend":"fake","id":"session-1"},"final_response":"Drafted"}`)
	calls := doc("tool-1", "tools/tool-1.json", `{"scope":{"role":"architect"},"name":"get_bundle","state":"completed","output":{"content":"external evidence"}}`, 1)
	events := []connector.Event{}
	for _, r := range []record{request, response, calls} {
		out, err := c.events(project, r, at)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	kinds := []connector.Kind{}
	for _, e := range events {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, e.Kind)
	}
	want := []connector.Kind{connector.KindAgentSession, connector.KindAgentTurn, connector.KindAgentSession, connector.KindToolCall}
	if !slices.Equal(kinds, want) {
		t.Fatalf("audit kinds %v", kinds)
	}
	if !strings.Contains(string(events[1].Payload.Native), "request-1") || !strings.Contains(events[1].Payload.Text, "session-1") {
		t.Fatal("lost turn correlation")
	}
	if events[0].Payload.Author.NativeID != "worker" {
		t.Fatal("role identity is not mapped to its class")
	}
}

func (s *capture) CurrentArtifact(ctx context.Context, source, artifact string) (connector.Event, bool, error) {
	events, err := s.CurrentArtifacts(ctx, source)
	if err != nil {
		return connector.Event{}, false, err
	}
	for _, event := range events {
		if event.Payload.Artifact == artifact {
			return event, true, nil
		}
	}
	return connector.Event{}, false, nil
}

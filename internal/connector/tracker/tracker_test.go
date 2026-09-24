package tracker_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/tracker"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

const (
	sourceID = "linear"
	token    = "sender-secret"
)

// source is ENG, which everyone may read, and SEC, which the security group
// may, with the settings given where they are not nil.
func source(settings map[string]any) connector.SourceConfig {
	if settings == nil {
		settings = map[string]any{}
	}
	if _, ok := settings["access"]; !ok {
		settings["access"] = map[string]any{
			"ENG": []map[string]string{{"kind": "public"}},
			"SEC": []map[string]string{{"kind": "group", "native_id": "security"}},
		}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		panic(err)
	}
	return connector.SourceConfig{
		ID: sourceID, Type: tracker.Type, Containers: []string{"ENG", "SEC"},
		Settings: raw, Secrets: map[string]string{tracker.SecretToken: token},
	}
}

// server is the connector's handler behind the gate the runtime would give it,
// writing to a recorder.
func server(t *testing.T, src connector.SourceConfig) (*httptest.Server, *connector.Recorder, *tracker.Connector) {
	t.Helper()
	c, err := tracker.New(src)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	srv := httptest.NewServer(c.Handler(gate))
	t.Cleanup(srv.Close)
	return srv, rec, c
}

func post(t *testing.T, url string, header http.Header, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(out)
}

func bearer(tok string) http.Header {
	return http.Header{"Authorization": {"Bearer " + tok}, "Content-Type": {"application/json"}}
}

// send posts with the source's token and fails the test on an unexpected status.
func send(t *testing.T, url, body string, want int) string {
	t.Helper()
	code, out := post(t, url, bearer(token), body)
	if code != want {
		t.Fatalf("POST %s = %d %s, want %d", body, code, out, want)
	}
	return out
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// with returns a fixture with its top-level fields replaced; a nil value
// removes one.
func with(t *testing.T, body string, fields map[string]any) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestNewRefusesASourceItCannotServe(t *testing.T) {
	access := func(acl ...map[string]string) map[string]any {
		return map[string]any{"ENG": []map[string]string{{"kind": "public"}}, "SEC": acl}
	}
	tests := []struct {
		name   string
		mutate func(*connector.SourceConfig)
		want   string
	}{
		{"no token", func(s *connector.SourceConfig) { s.Secrets = nil }, "secrets.token is required"},
		{"a secret it does not take", func(s *connector.SourceConfig) { s.Secrets["webhook_secret"] = "x" }, `"webhook_secret" is not one`},
		{"no containers", func(s *connector.SourceConfig) { s.Containers = nil }, "containers must name"},
		{"every project", func(s *connector.SourceConfig) { s.Containers = []string{"*"} }, `may not be "*"`},
		{"a project key an artifact cannot hold", func(s *connector.SourceConfig) { s.Containers = []string{"ENG", "SEC#1"} }, "is not a project key"},
		{"a project with no access", func(s *connector.SourceConfig) { s.Containers = append(s.Containers, "OPS") }, `project "OPS" has no entry in settings.access`},
		{"access for a project that is not a container", func(s *connector.SourceConfig) { s.Containers = []string{"ENG"} }, `names project "SEC", which is not in containers`},
		{"an empty access list", func(s *connector.SourceConfig) { *s = source(map[string]any{"access": access()}) }, "access list is empty"},
		{"a group with no id", func(s *connector.SourceConfig) {
			*s = source(map[string]any{"access": access(map[string]string{"kind": "group"})})
		}, "group with no native_id"},
		{"a public entry naming someone", func(s *connector.SourceConfig) {
			*s = source(map[string]any{"access": access(map[string]string{"kind": "public", "native_id": "x"})})
		}, "is public and names"},
		{"a kind that is not a grant", func(s *connector.SourceConfig) {
			*s = source(map[string]any{"access": access(map[string]string{"kind": "everyone"})})
		}, `kind "everyone"`},
		{"a setting it does not have", func(s *connector.SourceConfig) { *s = source(map[string]any{"ticket_acls": true}) }, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := source(nil)
			tt.mutate(&src)
			_, err := tracker.New(src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// Nothing is read or written for a request that does not carry the source's
// token, exactly once, as a bearer token.
func TestOnlyTheSendersTokenIsAccepted(t *testing.T) {
	srv, rec, _ := server(t, source(nil))
	ticket := fixture(t, "ticket.json")
	tests := []struct {
		name   string
		header http.Header
		want   int
	}{
		{"no token", http.Header{}, http.StatusUnauthorized},
		{"another token", bearer("guess"), http.StatusUnauthorized},
		{"the token with something after it", bearer(token + "x"), http.StatusUnauthorized},
		{"the token as basic auth", http.Header{"Authorization": {"Basic " + token}}, http.StatusUnauthorized},
		{"the token twice", http.Header{"Authorization": {"Bearer " + token, "Bearer " + token}}, http.StatusUnauthorized},
		{"the token", bearer(token), http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(rec.Events())
			code, body := post(t, srv.URL, tt.header, ticket)
			if code != tt.want {
				t.Fatalf("POST = %d %s, want %d", code, body, tt.want)
			}
			if got := len(rec.Events()) - before; (got == 1) != (code == http.StatusAccepted) {
				t.Fatalf("POST answered %d and emitted %d events", code, got)
			}
		})
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer(token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", resp.StatusCode)
	}
}

// A request that is not a ticket or a comment is refused with what is wrong
// with it, and a project the source does not grant is forbidden.
func TestRequestsAreValidated(t *testing.T) {
	srv, rec, _ := server(t, source(nil))
	ticket, comment := fixture(t, "ticket.json"), fixture(t, "comment.json")
	tests := []struct {
		name string
		body string
		want int
		says string
	}{
		{"not JSON", "ticket", http.StatusBadRequest, "not a ticket or a comment"},
		{"two requests", ticket + ticket, http.StatusBadRequest, "one ticket or one comment"},
		{"a field the shape does not have", with(t, ticket, map[string]any{"priority": "high"}), http.StatusBadRequest, "unknown field"},
		{"no kind", with(t, ticket, map[string]any{"kind": nil}), http.StatusBadRequest, "kind must be ticket or comment"},
		{"a kind it does not take", with(t, ticket, map[string]any{"kind": "epic"}), http.StatusBadRequest, "kind must be ticket or comment"},
		{"no project", with(t, ticket, map[string]any{"project": nil}), http.StatusBadRequest, "project and id"},
		{"an id that would split an artifact", with(t, ticket, map[string]any{"id": "ENG#42"}), http.StatusBadRequest, "project and id"},
		{"an id with a space", with(t, ticket, map[string]any{"id": "ENG 42"}), http.StatusBadRequest, "project and id"},
		{"no title", with(t, ticket, map[string]any{"title": nil}), http.StatusBadRequest, "a ticket has a title"},
		{"no author", with(t, ticket, map[string]any{"author": nil}), http.StatusBadRequest, "author is required"},
		{"an author with no id", with(t, ticket, map[string]any{"author": map[string]string{"handle": "kyle"}}), http.StatusBadRequest, "author.id is required"},
		{"an author of another kind", with(t, ticket, map[string]any{"author": map[string]string{"id": "u-1", "kind": "agent"}}), http.StatusBadRequest, "author.kind"},
		{"an assignee with no id", with(t, ticket, map[string]any{"assignees": []map[string]string{{"handle": "robin"}}}), http.StatusBadRequest, "assignees[0].id"},
		{"no updated_at", with(t, ticket, map[string]any{"updated_at": nil}), http.StatusBadRequest, "created_at and updated_at"},
		{"updated before it was created", with(t, ticket, map[string]any{"updated_at": "2026-09-19T00:00:00Z"}), http.StatusBadRequest, "earlier than created_at"},
		{"a time that is not RFC 3339", with(t, ticket, map[string]any{"created_at": "yesterday"}), http.StatusBadRequest, "not a ticket or a comment"},
		{"a URL that is not one", with(t, ticket, map[string]any{"url": "tracker.example/ENG/42"}), http.StatusBadRequest, "url must be"},
		{"its own parent", with(t, ticket, map[string]any{"parent": map[string]string{"id": "ENG-42"}}), http.StatusBadRequest, "not its own parent"},
		{"a parent with no id", with(t, ticket, map[string]any{"parent": map[string]string{"project": "OPS"}}), http.StatusBadRequest, "parent's project and id"},
		{"a ticket naming a ticket", with(t, ticket, map[string]any{"ticket": "ENG-1"}), http.StatusBadRequest, "a ticket names no ticket"},
		{"an acl the source does not take", with(t, ticket, map[string]any{"acl": []map[string]string{{"kind": "public"}}}), http.StatusBadRequest, "takes no per-ticket acl"},
		{"a comment with no ticket", with(t, comment, map[string]any{"ticket": nil}), http.StatusBadRequest, "names its ticket"},
		{"a comment with no body", with(t, comment, map[string]any{"body": nil}), http.StatusBadRequest, "a comment has a body"},
		{"a comment with a title", with(t, comment, map[string]any{"title": "Re: retries"}), http.StatusBadRequest, "no title, status, parent or assignees"},
		{"a comment with an acl", with(t, comment, map[string]any{"acl": []map[string]string{{"kind": "public"}}}), http.StatusBadRequest, "no acl of its own"},
		{"a project the source does not ingest", with(t, ticket, map[string]any{"project": "OPS"}), http.StatusForbidden, `project "OPS" is not one`},
		{"a deletion in a project the source does not ingest", with(t, fixture(t, "delete.json"), map[string]any{"project": "OPS"}), http.StatusForbidden, `project "OPS" is not one`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := post(t, srv.URL, bearer(token), tt.body)
			if code != tt.want || !strings.Contains(body, tt.says) {
				t.Fatalf("POST = %d %q, want %d saying %q", code, body, tt.want, tt.says)
			}
		})
	}
	if n := len(rec.Events()); n != 0 {
		t.Fatalf("refused requests emitted %d events", n)
	}
}

// The worked example in docs/connector-contract.md is this fixture, and it is
// written as the event the document shows.
func TestATicketIsAnIssue(t *testing.T) {
	srv, rec, _ := server(t, source(nil))
	out := send(t, srv.URL, fixture(t, "ticket.json"), http.StatusAccepted)

	const nativeID = "ENG#ENG-42@666042d1a079e88b"
	want := connector.Event{
		ID:       connector.EventID(sourceID, nativeID),
		Source:   sourceID,
		NativeID: nativeID,
		Kind:     connector.KindIssue,
		Time:     time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC),
		Payload: connector.Payload{
			Artifact:  "ENG#ENG-42",
			Container: connector.Container{Kind: tracker.ContainerProject, NativeID: "ENG", Name: "ENG"},
			URL:       "https://tracker.example/ENG/42",
			Title:     "Retry webhook deliveries with backoff",
			Text:      "Failed deliveries are dropped today. Retry them with exponential backoff.",
			Author:    &connector.Identity{Source: sourceID, Kind: connector.IdentityUser, NativeID: "u-17", Handle: "kyle", DisplayName: "Kyle"},
			PartOf:    "ENG#ENG-7",
			Revision:  &connector.Revision{Token: "666042d1a079e88b", EditedAt: time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)},
			Native:    json.RawMessage(`{"project":"ENG","id":"ENG-42","status":"In Progress","assignees":[{"source":"linear","kind":"user","native_id":"u-23","handle":"robin"}]}`),
		},
		ACL: connector.ACL{{Kind: connector.ACLPublic}},
	}
	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(events))
	}
	if got := events[0]; !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("emitted\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	if !strings.Contains(out, want.ID) {
		t.Fatalf("answered %s, want the event id %s", out, want.ID)
	}
}

// Posting a revision again is the same event, however the JSON is laid out;
// anything the event says changing is a new revision of the same artifact.
func TestRevisionsAreStable(t *testing.T) {
	srv, rec, _ := server(t, source(nil))
	ticket := fixture(t, "ticket.json")
	first := send(t, srv.URL, ticket, http.StatusAccepted)

	tests := []struct {
		name string
		body string
		same bool
	}{
		{"the same request", ticket, true},
		{"the same request laid out differently", with(t, ticket, nil), true},
		{"a new title", with(t, ticket, map[string]any{"title": "Retry deliveries"}), false},
		{"a new status", with(t, ticket, map[string]any{"status": "Done"}), false},
		{"a new parent", with(t, ticket, map[string]any{"parent": map[string]string{"id": "ENG-8"}}), false},
		{"no assignee", with(t, ticket, map[string]any{"assignees": nil}), false},
		{"a later update with nothing else changed", with(t, ticket, map[string]any{"updated_at": "2026-09-24T00:00:00Z"}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := send(t, srv.URL, tt.body, http.StatusAccepted)
			if (out == first) != tt.same {
				t.Fatalf("answered %s after %s, want the same id: %v", out, first, tt.same)
			}
		})
	}
	for _, ev := range rec.Events() {
		if ev.Payload.Artifact != "ENG#ENG-42" {
			t.Fatalf("a revision is of artifact %q, want every one of ENG#ENG-42", ev.Payload.Artifact)
		}
	}
}

// A ticket's parent is part_of, and the tracker hierarchy and the document
// read it with nothing tracker-specific: a scope mapping the project makes the
// ticket and its parent tracker items, and the comment part of the ticket's
// document.
func TestParentsAndCommentsReadAsATrackersDo(t *testing.T) {
	srv, rec, _ := server(t, source(nil))
	send(t, srv.URL, fixture(t, "ticket.json"), http.StatusAccepted)
	send(t, srv.URL, fixture(t, "comment.json"), http.StatusAccepted)
	send(t, srv.URL, with(t, fixture(t, "ticket.json"), map[string]any{"id": "ENG-43", "parent": map[string]string{"project": "SEC", "id": "SEC-1"}}), http.StatusAccepted)
	events := rec.Events()
	ticket, comment, crossProject := events[0], events[1], events[2]

	repo := config.Repo{Scopes: []config.Scope{
		{ID: "eng", Sources: []config.ScopeSource{{Source: sourceID, Containers: []string{"ENG"}}}, Tracker: config.SourceRef{Source: sourceID, Project: "ENG"}},
		{ID: "sec", Sources: []config.ScopeSource{{Source: sourceID, Containers: []string{"SEC"}}}, Tracker: config.SourceRef{Source: sourceID, Project: "SEC"}},
	}}
	tests := []struct {
		name       string
		ev         connector.Event
		wantItem   string
		wantParent []string
	}{
		{"a parent in the project", ticket, "tracker:linear:ENG#ENG-42", []string{"tracker:linear:ENG#ENG-7"}},
		{"a parent in another project", crossProject, "tracker:linear:ENG#ENG-43", []string{"tracker:linear:SEC#SEC-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := l2.PlacementOf(repo, tt.ev)
			if !ok || p.Item.ID != tt.wantItem || !slices.Equal(p.ParentIDs(), tt.wantParent) {
				t.Fatalf("PlacementOf = %v %v under %v, want %s under %v", ok, p.Item.ID, p.ParentIDs(), tt.wantItem, tt.wantParent)
			}
		})
	}

	if comment.Kind != connector.KindMessage || comment.Payload.Parent != "ENG#ENG-42" || comment.Payload.Thread != "ENG#ENG-42" {
		t.Fatalf("the comment is %s off %q in thread %q, want a message off and in ENG#ENG-42", comment.Kind, comment.Payload.Parent, comment.Payload.Thread)
	}
	doc, err := l1.Build(l1.Input{Root: ticket, Children: []connector.Event{comment}, Repo: repo})
	if err != nil {
		t.Fatalf("l1.Build = %v", err)
	}
	if doc.ID != l1.DocID(sourceID, "ENG#ENG-42") || doc.Kind != l1.KindIssue || len(doc.L0Refs) != 2 || !slices.Contains(doc.Scope, "tracker:linear:ENG#ENG-42") {
		t.Fatalf("the document is %s, a %s of %d events about %v; want the ticket's issue with its comment, about its tracker item", doc.ID, doc.Kind, len(doc.L0Refs), doc.Scope)
	}
}

// A deletion retracts the current revision with a tombstone carrying its
// container, time and access list; deleting what is not held emits nothing.
func TestDeletionsAreTombstones(t *testing.T) {
	srv, rec, _ := server(t, source(nil))
	ticket, deletion := fixture(t, "ticket.json"), fixture(t, "delete.json")

	send(t, srv.URL, deletion, http.StatusNoContent)
	if n := len(rec.Events()); n != 0 {
		t.Fatalf("deleting a ticket Hearsay never held emitted %d events", n)
	}

	send(t, srv.URL, ticket, http.StatusAccepted)
	held := rec.Events()[0]
	send(t, srv.URL, deletion, http.StatusAccepted)
	tomb := rec.Events()[1]
	switch {
	case tomb.Kind != connector.KindTombstone || tomb.Payload.Target != "ENG#ENG-42":
		t.Fatalf("the deletion emitted a %s of %q, want a tombstone of ENG#ENG-42", tomb.Kind, tomb.Payload.Target)
	case !strings.HasPrefix(tomb.Payload.Artifact, "ENG#ENG-42:tombstone:"):
		t.Fatalf("the tombstone is artifact %q, want one of its own named after the ticket", tomb.Payload.Artifact)
	case !tomb.Time.Equal(held.Time) || !reflect.DeepEqual(tomb.ACL, held.ACL) || tomb.Payload.Container != held.Payload.Container:
		t.Fatalf("the tombstone is %v in %v readable by %v, want the retracted revision's %v, %v, %v", tomb.Time, tomb.Payload.Container, tomb.ACL, held.Time, held.Payload.Container, held.ACL)
	}

	send(t, srv.URL, deletion, http.StatusNoContent)
	if n := len(rec.Events()); n != 2 {
		t.Fatalf("deleting a ticket again emitted %d more events", n-2)
	}

	// The ticket comes back, and goes again: a second retraction, not a replay
	// of the first.
	send(t, srv.URL, with(t, ticket, map[string]any{"updated_at": "2026-09-25T00:00:00Z"}), http.StatusAccepted)
	send(t, srv.URL, deletion, http.StatusAccepted)
	if again := rec.Events()[3]; again.Kind != connector.KindTombstone || again.ID == tomb.ID {
		t.Fatalf("deleting the returned ticket emitted %s %s, want a new tombstone", again.Kind, again.ID)
	}

	// A comment is deleted the same way.
	send(t, srv.URL, fixture(t, "comment.json"), http.StatusAccepted)
	send(t, srv.URL, `{"kind":"comment","project":"ENG","ticket":"ENG-42","id":"9001","deleted":true}`, http.StatusAccepted)
	if last := rec.Events()[5]; last.Kind != connector.KindTombstone || last.Payload.Target != "ENG#ENG-42:comment:9001" {
		t.Fatalf("deleting the comment emitted a %s of %q", last.Kind, last.Payload.Target)
	}
}

// Access is the project's unless the source lets a ticket carry its own, and a
// comment is read by whoever may read its ticket.
func TestAccessIsTheProjectsOrTheTickets(t *testing.T) {
	security := connector.ACL{{Kind: connector.ACLGroup, Source: sourceID, NativeID: "security"}}
	ownACL := []map[string]string{{"kind": "identity", "native_id": "u-17"}, {"kind": "group", "source": "github", "native_id": "acme/leads"}}
	own := connector.ACL{{Kind: connector.ACLIdentity, Source: sourceID, NativeID: "u-17"}, {Kind: connector.ACLGroup, Source: "github", NativeID: "acme/leads"}}
	ticket, comment := fixture(t, "ticket.json"), fixture(t, "comment.json")
	inSEC := map[string]any{"project": "SEC", "id": "SEC-42", "parent": nil}
	commentInSEC := map[string]any{"project": "SEC", "ticket": "SEC-42"}

	tests := []struct {
		name      string
		ticketACL bool
		posts     []string
		want      int
		wantACL   connector.ACL
	}{
		{"a public project's ticket", false, []string{ticket}, http.StatusAccepted, connector.ACL{{Kind: connector.ACLPublic}}},
		{"a restricted project's ticket", false, []string{with(t, ticket, inSEC)}, http.StatusAccepted, security},
		{"a restricted project's comment, before its ticket", false, []string{with(t, comment, commentInSEC)}, http.StatusAccepted, security},
		{"a ticket with no acl of its own", true, []string{with(t, ticket, inSEC)}, http.StatusAccepted, security},
		{"a ticket with its own acl", true, []string{with(t, ticket, map[string]any{"acl": ownACL})}, http.StatusAccepted, own},
		{"a comment on a ticket with its own acl", true, []string{with(t, ticket, map[string]any{"acl": ownACL}), comment}, http.StatusAccepted, own},
		{"a comment on a ticket Hearsay does not hold", true, []string{comment}, http.StatusConflict, nil},
		{"a ticket's own acl that is empty", true, []string{with(t, ticket, map[string]any{"acl": []any{}})}, http.StatusBadRequest, nil},
		{"a ticket's own acl naming nobody", true, []string{with(t, ticket, map[string]any{"acl": []map[string]string{{"kind": "identity"}}})}, http.StatusBadRequest, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec, _ := server(t, source(map[string]any{"ticket_acl": tt.ticketACL}))
			last := len(tt.posts) - 1
			for _, body := range tt.posts[:last] {
				send(t, srv.URL, body, http.StatusAccepted)
			}
			send(t, srv.URL, tt.posts[last], tt.want)
			events := rec.Events()
			if tt.wantACL == nil {
				if len(events) != last {
					t.Fatalf("a refused request emitted an event")
				}
				return
			}
			if got := events[len(events)-1].ACL; !reflect.DeepEqual(got, tt.wantACL) {
				t.Fatalf("acl = %v, want %v", got, tt.wantACL)
			}
		})
	}
}

// The connector reports when it last emitted, which is how a quiet sender is
// told from a broken one.
func TestHealthSaysWhenItLastEmitted(t *testing.T) {
	srv, _, c := server(t, source(nil))
	if h := c.Health(t.Context()); h.Status != connector.HealthOK || !h.LastEventAt.IsZero() {
		t.Fatalf("Health before anything = %+v", h)
	}
	send(t, srv.URL, fixture(t, "ticket.json"), http.StatusAccepted)
	if h := c.Health(t.Context()); h.LastEventAt.IsZero() {
		t.Fatalf("Health after a ticket = %+v, want a last event time", h)
	}
}

// docs/connector-contract.md's worked request is the fixture the tests post.
func TestTheContractShowsTheFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/connector-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ticket.json", "comment.json", "delete.json"} {
		if !strings.Contains(string(raw), strings.TrimSpace(fixture(t, name))) {
			t.Errorf("docs/connector-contract.md does not show testdata/%s verbatim", name)
		}
	}
}

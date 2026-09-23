package drive_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/drive"
)

type fixture struct {
	mu         sync.Mutex
	calls      []string
	badLabels  bool
	failLabels bool
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.RequestURI())
	f.mu.Unlock()
	if r.URL.Path == "/token" {
		if r.Method != "POST" || r.FormValue("assertion") == "" {
			http.Error(w, "missing assertion", 400)
			return
		}
		fmt.Fprint(w, `{"access_token":"fixture-token","expires_in":3600}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer fixture-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/drive/v3/files":
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "'docs'") {
			if r.URL.Query().Get("pageToken") == "next" {
				fmt.Fprint(w, `{"files":[{"id":"doc2","name":"Second","mimeType":"text/plain","parents":["docs"],"headRevisionId":"r2","createdTime":"2026-01-01T00:00:00Z","modifiedTime":"2026-01-02T00:00:00Z","owners":[{"permissionId":"u1","emailAddress":"a@example.com"}]}]}`)
			} else {
				fmt.Fprint(w, `{"nextPageToken":"next","files":[{"id":"doc1","name":"Design","mimeType":"text/plain","parents":["docs"],"headRevisionId":"r1","createdTime":"2026-01-01T00:00:00Z","modifiedTime":"2026-01-02T00:00:00Z","webViewLink":"https://drive.google.com/open?id=doc1","owners":[{"permissionId":"u1","emailAddress":"a@example.com"}],"properties":{"calendar_attendee_emails":"b@example.com"}},{"id":"nested","name":"Nested","mimeType":"text/plain","parents":["child"],"headRevisionId":"r1","createdTime":"2026-01-01T00:00:00Z","owners":[{"permissionId":"u1"}]}]}`)
			}
			return
		}
		fmt.Fprint(w, `{"files":[{"id":"tagged","name":"Notes","mimeType":"application/vnd.google-apps.document","parents":["meet"],"createdTime":"2026-01-01T00:00:00Z","modifiedTime":"2026-01-02T00:00:00Z","owners":[{"permissionId":"u1"}]},{"id":"untagged","name":"Notes","mimeType":"text/plain","parents":["meet"],"headRevisionId":"r1","createdTime":"2026-01-01T00:00:00Z"},{"id":"other","name":"Notes","mimeType":"text/plain","parents":["meet"],"headRevisionId":"r1","createdTime":"2026-01-01T00:00:00Z"},{"id":"unreadable","name":"Notes","mimeType":"text/plain","parents":["meet"],"headRevisionId":"r1","createdTime":"2026-01-01T00:00:00Z"}]}`)
	case strings.HasSuffix(r.URL.Path, "/listLabels"):
		if strings.Contains(r.URL.Path, "unreadable") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "tagged") && !strings.Contains(r.URL.Path, "untagged") {
			f.mu.Lock()
			bad, fail := f.badLabels, f.failLabels
			f.mu.Unlock()
			if fail {
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
			if bad {
				fmt.Fprint(w, `{"labels":[{"id":"label"},{"id":"label"}]}`)
			} else {
				fmt.Fprint(w, `{"labels":[{"id":"label"}]}`)
			}
		} else if strings.Contains(r.URL.Path, "other") {
			fmt.Fprint(w, `{"labels":[{"id":"different"}]}`)
		} else {
			fmt.Fprint(w, `{"labels":[]}`)
		}
	case strings.HasSuffix(r.URL.Path, "/revisions"):
		fmt.Fprint(w, `{"revisions":[{"id":"old"},{"id":"head"}]}`)
	case strings.HasSuffix(r.URL.Path, "/permissions"):
		fmt.Fprint(w, `{"permissions":[{"id":"u1","type":"user","emailAddress":"a@example.com"},{"id":"g1","type":"group","emailAddress":"team@example.com"},{"type":"domain","domain":"example.com"}]}`)
	case strings.HasSuffix(r.URL.Path, "/export"):
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "meeting text")
	case r.URL.Query().Get("alt") == "media":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "original text")
	default:
		http.Error(w, "unexpected path", 404)
	}
}

func source(t *testing.T, server string) connector.SourceConfig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "service@example.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": server + "/token"})
	return connector.SourceConfig{ID: "drive-fixture", Type: drive.Type, Containers: []string{"docs", "meet"}, Settings: json.RawMessage(`{"transcript_candidate_folder_ids":["meet"],"meeting_transcript_label_id":"label","api_url":"` + server + `/drive/v3"}`), Secrets: map[string]string{"credentials": string(cred)}}
}

func walk(t *testing.T, c *drive.Connector, sink connector.Sink, from connector.Cursor) (connector.Cursor, bool) {
	t.Helper()
	result, err := c.Backfill(t.Context(), sink, from)
	if err != nil {
		t.Fatal(err)
	}
	return result.Next, result.Done
}

func TestBackfillRestartAndGate(t *testing.T) {
	f := &fixture{}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := source(t, server.URL)
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	cur, done := walk(t, c, gate, "")
	if done || cur == "" {
		t.Fatalf("first cursor = %q, done %v", cur, done)
	}
	c, err = drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	gate = connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	for !done {
		cur, done = walk(t, c, gate, cur)
	}
	events := rec.Events()
	if len(events) != 3 {
		t.Fatalf("events = %+v, want 3", events)
	}
	ids := []string{}
	for _, ev := range events {
		ids = append(ids, ev.NativeID)
	}
	if want := []string{"doc1@r1", "doc2@r2", "tagged@head"}; !slices.Equal(ids, want) {
		t.Errorf("native IDs = %v, want %v", ids, want)
	}
	if events[0].Kind != connector.KindDocument || events[2].Kind != connector.KindTranscript || events[2].Payload.Container.NativeID != "meet" {
		t.Errorf("kinds and containers = %+v", events)
	}
	if events[0].Payload.URL != "https://drive.google.com/open?id=doc1" || events[0].Payload.Text != "original text" || len(events[0].Payload.Participants) != 1 || events[0].Payload.Participants[0].Role != connector.RoleAttendee {
		t.Errorf("document payload = %+v", events[0].Payload)
	}
	if events[0].ACL[0].Kind != connector.ACLIdentity || events[0].ACL[1].Kind != connector.ACLGroup || events[0].ACL[2].Kind != connector.ACLPublic {
		t.Errorf("ACL = %+v", events[0].ACL)
	}
	if rec.IDs()[0] != connector.EventID(src.ID, "doc1@r1") {
		t.Errorf("gate did not stamp event ID: %v", rec.IDs())
	}
}

func TestCandidateMetadataFailure(t *testing.T) {
	for _, tt := range []struct {
		name       string
		bad, fail  bool
		wantError  bool
		wantEvents int
	}{{"ambiguous", true, false, false, 2}, {"transient", false, true, true, 2}} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fixture{badLabels: tt.bad, failLabels: tt.fail}
			server := httptest.NewServer(http.HandlerFunc(f.serve))
			defer server.Close()
			src := source(t, server.URL)
			c, err := drive.New(src)
			if err != nil {
				t.Fatal(err)
			}
			rec := &connector.Recorder{}
			gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
			cur, _ := walk(t, c, gate, "")
			cur, _ = walk(t, c, gate, cur)
			_, err = c.Backfill(t.Context(), gate, cur)
			if (err != nil) != tt.wantError {
				t.Errorf("Backfill error = %v, want error %v", err, tt.wantError)
			}
			if len(rec.Events()) != tt.wantEvents {
				t.Errorf("events = %v, want %d", rec.IDs(), tt.wantEvents)
			}
		})
	}
}

func TestConfigurationRejectsUnsafeFolders(t *testing.T) {
	f := &fixture{}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	for _, tt := range []struct {
		name       string
		containers []string
		settings   string
		secret     bool
	}{{"wildcard", []string{"*"}, `{}`, false}, {"outside", []string{"docs"}, `{"transcript_candidate_folder_ids":["meet"],"meeting_transcript_label_id":"label"}`, false}, {"missing label", []string{"docs", "meet"}, `{"transcript_candidate_folder_ids":["meet"]}`, false}, {"duplicate candidate", []string{"docs", "meet"}, `{"transcript_candidate_folder_ids":["meet","meet"],"meeting_transcript_label_id":"label"}`, false}, {"overlap by duplicate container", []string{"docs", "docs"}, `{}`, false}, {"bad credential", []string{"docs"}, `{}`, true}} {
		t.Run(tt.name, func(t *testing.T) {
			src := source(t, server.URL)
			src.Containers = tt.containers
			src.Settings = json.RawMessage(tt.settings)
			if tt.secret {
				src.Secrets["credentials"] = "bad"
			}
			if _, err := drive.New(src); err == nil {
				t.Error("New accepted unsafe config")
			}
		})
	}
}

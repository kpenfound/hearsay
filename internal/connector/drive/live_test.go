package drive_test

import (
	"encoding/json"
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

type liveFile struct {
	Folder, Head, Text      string
	Tagged, Public, Deleted bool
	LabelID                 string
	UnreadableLabels        bool
}

type liveFixture struct {
	mu                  sync.Mutex
	files               map[string]liveFile
	changes             map[string][]string
	start               string
	watchID, watchToken string
	failWatch           bool
}

type wakeSink struct {
	connector.Sink
	calls int
}

func (w *wakeSink) RequestPoll() { w.calls++ }

func (f *liveFixture) set(id string, state liveFile) { f.mu.Lock(); f.files[id] = state; f.mu.Unlock() }
func (f *liveFixture) next(from, to string, ids ...string) {
	f.mu.Lock()
	f.changes[from] = ids
	f.start = to
	f.mu.Unlock()
}

func (f *liveFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		fmt.Fprint(w, `{"access_token":"fixture-token","expires_in":3600}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer fixture-token" {
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/drive/v3")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case path == "/changes/startPageToken":
		write(map[string]string{"startPageToken": f.start})
	case path == "/changes/watch":
		if f.failWatch {
			http.Error(w, "watch unavailable", http.StatusForbidden)
			return
		}
		var request struct{ ID, Token string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "body", 400)
			return
		}
		f.watchID, f.watchToken = request.ID, request.Token
		write(map[string]string{"expiration": "1790000000000"})
	case path == "/changes":
		from := r.URL.Query().Get("pageToken")
		ids, ok := f.changes[from]
		if !ok {
			http.Error(w, "unknown token", http.StatusGone)
			return
		}
		changes := []map[string]any{}
		for _, id := range ids {
			changes = append(changes, map[string]any{"fileId": id, "changeType": "file", "removed": f.files[id].Deleted})
		}
		write(map[string]any{"changes": changes, "newStartPageToken": f.start})
	case path == "/files":
		folder := ""
		for _, candidate := range []string{"docs", "meet"} {
			if strings.Contains(r.URL.Query().Get("q"), "'"+candidate+"'") {
				folder = candidate
			}
		}
		files := []map[string]any{}
		for id, state := range f.files {
			if !state.Deleted && state.Folder == folder {
				files = append(files, fileJSON(id, state))
			}
		}
		slices.SortFunc(files, func(a, b map[string]any) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
		write(map[string]any{"files": files})
	case strings.HasPrefix(path, "/files/"):
		rest := strings.TrimPrefix(path, "/files/")
		id, suffix, _ := strings.Cut(rest, "/")
		state, ok := f.files[id]
		if !ok || state.Deleted {
			http.Error(w, "gone", 404)
			return
		}
		switch suffix {
		case "":
			if r.URL.Query().Get("alt") == "media" {
				fmt.Fprint(w, state.Text)
				return
			}
			write(fileJSON(id, state))
		case "listLabels":
			if state.UnreadableLabels {
				http.Error(w, "labels unavailable", http.StatusForbidden)
				return
			}
			labels := []map[string]string{}
			if state.Tagged {
				labels = append(labels, map[string]string{"id": "label"})
			} else if state.LabelID != "" {
				labels = append(labels, map[string]string{"id": state.LabelID})
			}
			write(map[string]any{"labels": labels})
		case "permissions":
			permissions := []map[string]string{{"id": "u1", "type": "user", "emailAddress": "a@example.com"}}
			if state.Public {
				permissions = append(permissions, map[string]string{"type": "anyone"})
			}
			write(map[string]any{"permissions": permissions})
		default:
			http.Error(w, "path", 404)
		}
	default:
		http.Error(w, "path", 404)
	}
}

func fileJSON(id string, f liveFile) map[string]any {
	return map[string]any{"id": id, "name": id, "mimeType": "text/plain", "parents": []string{f.Folder}, "headRevisionId": f.Head, "createdTime": "2026-01-01T00:00:00Z", "modifiedTime": "2026-01-02T00:00:00Z", "owners": []map[string]string{{"permissionId": "u1", "emailAddress": "a@example.com"}}}
}

func TestLiveChangesReplayRestartAndEligibility(t *testing.T) {
	f := &liveFixture{files: map[string]liveFile{
		"doc":      {Folder: "docs", Head: "r1", Text: "first", Public: true},
		"tagged":   {Folder: "meet", Head: "r1", Text: "meeting", Tagged: true, Public: true},
		"untagged": {Folder: "meet", Head: "r1", Text: "private", Public: true},
		"outside":  {Folder: "elsewhere", Head: "r1", Text: "outside", Public: true},
	}, changes: map[string][]string{"s0": {}}, start: "s0"}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := source(t, server.URL)
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	cursor, err := c.PollFrom(t.Context(), gate, "")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "s0" || len(rec.Events()) != 2 {
		t.Fatalf("initial cursor %s, events %+v", cursor, rec.Events())
	}
	assertCurrent := func(id, head, folder, text string, public bool) {
		t.Helper()
		ev, ok, err := gate.CurrentArtifact(t.Context(), src.ID, id)
		if err != nil || !ok {
			t.Fatalf("current %s: %v %v", id, ok, err)
		}
		if !strings.HasPrefix(ev.Payload.Revision.Token, head+"+perm:") || ev.Payload.Container.NativeID != folder || ev.Payload.Text != text {
			t.Errorf("current %s = %+v", id, ev)
		}
		foundPublic := false
		for _, a := range ev.ACL {
			if a.Kind == connector.ACLPublic {
				foundPublic = true
			}
		}
		if foundPublic != public {
			t.Errorf("current %s public = %v", id, foundPublic)
		}
	}
	f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "second", Public: true})
	f.next("s0", "s1", "doc", "untagged", "outside")
	cursor, err = c.PollFrom(t.Context(), gate, cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent("doc", "r2", "docs", "second", true)
	count := len(rec.Events())
	if _, err := c.PollFrom(t.Context(), gate, "s0"); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events()) != count {
		t.Errorf("replay emitted %d events", len(rec.Events())-count)
	}
	// A replacement connector resumes from the persisted token, including a
	// sharing change with no content revision.
	c, err = drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "second", Public: false})
	f.next("s1", "s2", "doc")
	cursor, err = c.PollFrom(t.Context(), gate, cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent("doc", "r2", "docs", "second", false)
	// Reopening the same ACL must append a new public revision, not select
	// the older public revision that preceded the tightening.
	f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "second", Public: true})
	f.next("s2", "s2b", "doc")
	cursor, err = c.PollFrom(t.Context(), gate, cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent("doc", "r2", "docs", "second", true)
	f.set("outside", liveFile{Folder: "docs", Head: "r1", Text: "outside", Public: true})
	f.set("doc", liveFile{Folder: "meet", Head: "r2", Text: "second", Tagged: true, Public: false})
	f.next("s2b", "s3", "outside", "doc")
	cursor, err = c.PollFrom(t.Context(), gate, cursor)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent("outside", "r1", "docs", "outside", true)
	assertCurrent("doc", "r2", "meet", "second", false)
	f.set("doc", liveFile{Folder: "meet", Head: "r2", Text: "second", Tagged: false, Public: false})
	f.set("tagged", liveFile{Folder: "meet", Head: "r1", Text: "meeting", Tagged: true, Public: true, Deleted: true})
	f.next("s3", "s4", "doc", "tagged")
	_, err = c.PollFrom(t.Context(), gate, cursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"doc", "tagged"} {
		_, ok, err := gate.CurrentArtifact(t.Context(), src.ID, id)
		if err != nil || ok {
			t.Errorf("%s remains current: %v %v", id, ok, err)
		}
	}
	for _, ev := range rec.Events() {
		if ev.Kind == connector.KindTombstone && ev.Payload.Container.NativeID != "meet" {
			t.Errorf("tombstone container = %+v", ev)
		}
	}
}

func TestNotificationWakesPollerAndExpiredTokenRecovers(t *testing.T) {
	f := &liveFixture{files: map[string]liveFile{"doc": {Folder: "docs", Head: "r1", Text: "one", Public: true}}, changes: map[string][]string{"s0": {}}, start: "s0"}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := source(t, server.URL)
	src.Settings = json.RawMessage(fmt.Sprintf(`{"transcript_candidate_folder_ids":["meet"],"meeting_transcript_label_id":"label","api_url":%q,"notification_url":%q}`, server.URL+"/drive/v3", server.URL+"/hooks/drive"))
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	first, err := c.PollFrom(t.Context(), gate, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollFrom(t.Context(), gate, first); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	id, token := f.watchID, f.watchToken
	f.mu.Unlock()
	if id == "" || token == "" {
		t.Fatal("no Drive channel was created")
	}
	wake := &wakeSink{Sink: gate}
	// A watch registration failure leaves the polling feed usable.
	f.mu.Lock()
	f.failWatch = true
	f.mu.Unlock()
	fallback, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fallback.PollFrom(t.Context(), gate, first); err != nil || got != first {
		t.Fatalf("polling fallback = %q, %v", got, err)
	}
	for _, tt := range []struct {
		name, token string
		status      int
	}{{"valid", token, 204}, {"forged", "wrong", 401}} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/drive", nil)
			req.Header.Set("X-Goog-Channel-ID", id)
			req.Header.Set("X-Goog-Channel-Token", tt.token)
			req.Header.Set("X-Goog-Resource-State", "change")
			w := httptest.NewRecorder()
			c.Handler(wake).ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Errorf("status %d", w.Code)
			}
		})
	}
	if wake.calls != 1 {
		t.Errorf("valid notification woke poller %d times", wake.calls)
	}
	// The saved token has expired. A new token is captured before a full
	// reconciliation, which catches removals even with no notification.
	f.set("doc", liveFile{Folder: "docs", Head: "r1", Text: "one", Public: true, Deleted: true})
	f.mu.Lock()
	f.start = "s2"
	f.mu.Unlock()
	got, err := c.PollFrom(t.Context(), gate, "expired")
	if err != nil || got != "s2" {
		t.Fatalf("recovery cursor %s: %v", got, err)
	}
	_, ok, err := gate.CurrentArtifact(t.Context(), src.ID, "doc")
	if err != nil || ok {
		t.Errorf("removed file current: %v %v", ok, err)
	}
}

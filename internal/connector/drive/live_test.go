package drive_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/drive"
	"github.com/kpenfound/hearsay/internal/telemetry"
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
	attemptedToken      string
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
		var request struct{ ID, Token string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "body", 400)
			return
		}
		f.attemptedToken = request.Token
		if f.failWatch {
			http.Error(w, "watch unavailable", http.StatusForbidden)
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

type liveEnv struct {
	f      *liveFixture
	src    connector.SourceConfig
	c      *drive.Connector
	gate   *connector.Gate
	rec    *connector.Recorder
	cursor connector.Cursor
}

func startLive(t *testing.T, files map[string]liveFile) *liveEnv {
	t.Helper()
	f := &liveFixture{files: files, changes: map[string][]string{"s0": {}}, start: "s0"}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	src := source(t, server.URL)
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	env := &liveEnv{f: f, src: src, c: c, rec: rec, gate: connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))}
	env.cursor, err = c.PollFrom(t.Context(), env.gate, "")
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *liveEnv) restart(t *testing.T) {
	t.Helper()
	c, err := drive.New(e.src)
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
}

func (e *liveEnv) poll(t *testing.T, to string, ids ...string) {
	t.Helper()
	e.f.next(string(e.cursor), to, ids...)
	next, err := e.c.PollFrom(t.Context(), e.gate, e.cursor)
	if err != nil {
		t.Fatal(err)
	}
	e.cursor = next
}

func (e *liveEnv) current(t *testing.T, id string) (connector.Event, bool) {
	t.Helper()
	ev, ok, err := e.gate.CurrentArtifact(t.Context(), e.src.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return ev, ok
}

func isPublic(ev connector.Event) bool {
	return slices.ContainsFunc(ev.ACL, func(a connector.ACLEntry) bool { return a.Kind == connector.ACLPublic })
}

func defaultLiveFiles() map[string]liveFile {
	return map[string]liveFile{
		"doc":      {Folder: "docs", Head: "r1", Text: "first", Public: true},
		"tagged":   {Folder: "meet", Head: "r1", Text: "meeting", Tagged: true, Public: true},
		"untagged": {Folder: "meet", Head: "r1", Text: "private", Public: true},
		"outside":  {Folder: "elsewhere", Head: "r1", Text: "outside", Public: true},
	}
}

func TestInitialPollEmitsOnlyEligibleFiles(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	if env.cursor != "s0" {
		t.Errorf("cursor = %q", env.cursor)
	}
	ids := []string{}
	for _, ev := range env.rec.Events() {
		ids = append(ids, ev.Payload.Artifact)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"doc", "tagged"}) {
		t.Errorf("artifacts = %v", ids)
	}
}

func TestChangePageEmitsCurrentRevision(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	env.f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "second", Public: true})
	env.poll(t, "s1", "doc", "untagged", "outside")
	ev, ok := env.current(t, "doc")
	if !ok || !strings.HasPrefix(ev.Payload.Revision.Token, "r2+perm:") || ev.Payload.Text != "second" || ev.Payload.Container.NativeID != "docs" || !isPublic(ev) {
		t.Errorf("current doc = %+v", ev)
	}
	for _, id := range []string{"untagged", "outside"} {
		if _, ok := env.current(t, id); ok {
			t.Errorf("%s is current", id)
		}
	}
}

func TestReplayedChangePageEmitsNothing(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	env.f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "second", Public: true})
	env.poll(t, "s1", "doc")
	count := len(env.rec.Events())
	if _, err := env.c.PollFrom(t.Context(), env.gate, "s0"); err != nil {
		t.Fatal(err)
	}
	if got := len(env.rec.Events()); got != count {
		t.Errorf("replay emitted %d events", got-count)
	}
}

func TestPermissionChangeIsNewRevisionAfterRestart(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	before, _ := env.current(t, "doc")
	env.restart(t)
	env.f.set("doc", liveFile{Folder: "docs", Head: "r1", Text: "first", Public: false})
	env.poll(t, "s1", "doc")
	private, _ := env.current(t, "doc")
	if isPublic(private) || private.NativeID == before.NativeID {
		t.Fatalf("after losing public access: %+v", private)
	}
	env.f.set("doc", liveFile{Folder: "docs", Head: "r1", Text: "first", Public: true})
	env.poll(t, "s2", "doc")
	public, _ := env.current(t, "doc")
	if !isPublic(public) || public.NativeID == private.NativeID {
		t.Errorf("after regaining public access: %+v", public)
	}
}

func TestFileMovedIntoAllowlistedFolderIsEmitted(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	env.f.set("outside", liveFile{Folder: "docs", Head: "r1", Text: "outside", Public: true})
	env.poll(t, "s1", "outside")
	ev, ok := env.current(t, "outside")
	if !ok || ev.Payload.Container.NativeID != "docs" || ev.Payload.Text != "outside" || !isPublic(ev) {
		t.Errorf("current outside = %+v", ev)
	}
}

func TestFileMovedToCandidateFolderIsTranscriptWhenTagged(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	env.f.set("doc", liveFile{Folder: "meet", Head: "r2", Text: "second", Tagged: true, Public: false})
	env.poll(t, "s1", "doc")
	ev, ok := env.current(t, "doc")
	if !ok || ev.Kind != connector.KindTranscript || ev.Payload.Container.NativeID != "meet" || ev.Payload.Text != "second" || isPublic(ev) {
		t.Errorf("current doc = %+v", ev)
	}
}

func TestUntaggedFileIsRetractedFromItsFolder(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	env.f.set("tagged", liveFile{Folder: "meet", Head: "r1", Text: "meeting", Public: true})
	env.poll(t, "s1", "tagged")
	if _, ok := env.current(t, "tagged"); ok {
		t.Error("untagged file remains current")
	}
	assertTombstoneIn(t, env.rec, "meet")
}

func TestDeletedFileIsRetractedFromItsFolder(t *testing.T) {
	env := startLive(t, defaultLiveFiles())
	env.f.set("tagged", liveFile{Folder: "meet", Head: "r1", Text: "meeting", Tagged: true, Public: true, Deleted: true})
	env.poll(t, "s1", "tagged")
	if _, ok := env.current(t, "tagged"); ok {
		t.Error("deleted file remains current")
	}
	assertTombstoneIn(t, env.rec, "meet")
}

func assertTombstoneIn(t *testing.T, rec *connector.Recorder, folder string) {
	t.Helper()
	found := false
	for _, ev := range rec.Events() {
		if ev.Kind != connector.KindTombstone {
			continue
		}
		found = true
		if ev.Payload.Container.NativeID != folder {
			t.Errorf("tombstone container = %+v", ev)
		}
	}
	if !found {
		t.Error("no tombstone was emitted")
	}
}

type watchEnv struct {
	f       *liveFixture
	src     connector.SourceConfig
	c       *drive.Connector
	gate    *connector.Gate
	cursor  connector.Cursor
	channel string
	token   string
}

func startWatched(t *testing.T) *watchEnv {
	t.Helper()
	f := &liveFixture{files: map[string]liveFile{"doc": {Folder: "docs", Head: "r1", Text: "one", Public: true}}, changes: map[string][]string{"s0": {}}, start: "s0"}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	src := source(t, server.URL)
	src.Settings = json.RawMessage(fmt.Sprintf(`{"transcript_candidate_folder_ids":["meet"],"meeting_transcript_label_id":"label","api_url":%q,"notification_url":%q}`, server.URL+"/drive/v3", server.URL+"/hooks/drive"))
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	gate := connector.NewGate(&connector.Recorder{}, src.ID, c.Describe(), connector.NewAllowlist(src))
	first, err := c.PollFrom(t.Context(), gate, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollFrom(t.Context(), gate, first); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchID == "" || f.watchToken == "" {
		t.Fatal("no Drive channel was created")
	}
	return &watchEnv{f: f, src: src, c: c, gate: gate, cursor: first, channel: f.watchID, token: f.watchToken}
}

func (w *watchEnv) notify(t *testing.T, sink connector.Sink, token string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/drive", nil)
	req.Header.Set("X-Goog-Channel-ID", w.channel)
	req.Header.Set("X-Goog-Channel-Token", token)
	req.Header.Set("X-Goog-Resource-State", "change")
	rec := httptest.NewRecorder()
	w.c.Handler(sink).ServeHTTP(rec, req)
	return rec.Code
}

func TestNotificationFromActiveChannelWakesPoller(t *testing.T) {
	env := startWatched(t)
	wake := &wakeSink{Sink: env.gate}
	if code := env.notify(t, wake, env.token); code != http.StatusNoContent {
		t.Errorf("status %d", code)
	}
	if wake.calls != 1 {
		t.Errorf("woke poller %d times", wake.calls)
	}
}

func TestNotificationWithWrongChannelTokenIsRefused(t *testing.T) {
	env := startWatched(t)
	wake := &wakeSink{Sink: env.gate}
	if code := env.notify(t, wake, "wrong"); code != http.StatusUnauthorized {
		t.Errorf("status %d", code)
	}
	if wake.calls != 0 {
		t.Errorf("forged notification woke poller %d times", wake.calls)
	}
}

func TestWatchFailureLeavesPollingUsable(t *testing.T) {
	env := startWatched(t)
	env.f.mu.Lock()
	env.f.failWatch = true
	env.f.mu.Unlock()
	fallback, err := drive.New(env.src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fallback.PollFrom(t.Context(), env.gate, env.cursor)
	if err != nil || got != env.cursor {
		t.Fatalf("polling fallback = %q, %v", got, err)
	}
}

func TestWatchFailureLogOmitsCredentials(t *testing.T) {
	env := startWatched(t)
	env.f.mu.Lock()
	env.f.failWatch = true
	env.f.mu.Unlock()
	fallback, err := drive.New(env.src)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if _, err := fallback.PollFrom(ctx, env.gate, env.cursor); err != nil {
		t.Fatal(err)
	}
	env.f.mu.Lock()
	attempted := env.f.attemptedToken
	env.f.mu.Unlock()
	if !strings.Contains(logs.String(), "Drive notification watch unavailable") {
		t.Fatalf("watch failure was not logged: %q", logs.String())
	}
	for _, secret := range []string{"fixture-token", attempted, env.token} {
		if secret == "" || strings.Contains(logs.String(), secret) {
			t.Errorf("log output holds %q: %s", secret, logs.String())
		}
	}
}

func TestExpiredChangeTokenRecoversByReconciling(t *testing.T) {
	env := startWatched(t)
	env.f.set("doc", liveFile{Folder: "docs", Head: "r1", Text: "one", Public: true, Deleted: true})
	env.f.mu.Lock()
	env.f.start = "s2"
	env.f.mu.Unlock()
	got, err := env.c.PollFrom(t.Context(), env.gate, "expired")
	if err != nil || got != "s2" {
		t.Fatalf("recovery cursor %s: %v", got, err)
	}
	if _, ok, err := env.gate.CurrentArtifact(t.Context(), env.src.ID, "doc"); err != nil || ok {
		t.Errorf("removed file current: %v %v", ok, err)
	}
}

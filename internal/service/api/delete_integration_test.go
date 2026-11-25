//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

type deleteWorld struct {
	src, other string
	url        string
	pool       *pgxpool.Pool
	hooks      map[string]*httptest.Server
	tokens     map[string]string

	mu       sync.Mutex
	withhold func(r *http.Request) bool
}

func newDeleteWorld(t *testing.T) *deleteWorld {
	t.Helper()
	tokens := map[string]string{"kyle": "test-kyle-api-credential", "sam": "test-sam-api-credential", "kern": "test-kern-api-credential",
		"stray": "test-stray-api-credential", "peek": "test-peek-api-credential"}
	for name, token := range tokens {
		t.Setenv("HEARSAY_TEST_"+strings.ToUpper(name)+"_TOKEN", token)
	}
	pool := dbtest.Pool(t)
	w := &deleteWorld{src: dbtest.UniqueID() + "k", other: dbtest.UniqueID() + "s", pool: pool, hooks: map[string]*httptest.Server{}, tokens: tokens}
	ids := func(id string) []principal.Identity {
		return []principal.Identity{{Source: w.src, NativeID: id}, {Source: w.other, NativeID: id}}
	}
	all := principal.Grant{Scopes: principal.AllScopes()}
	principals := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: all, TokenEnv: "HEARSAY_TEST_KYLE_TOKEN", Identities: ids("kyle")},
		{ID: "sam", Kind: principal.KindHuman, Grant: all, TokenEnv: "HEARSAY_TEST_SAM_TOKEN", Identities: ids("sam")},
		{ID: "kern", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: all, TokenEnv: "HEARSAY_TEST_KERN_TOKEN", Identities: ids("kern")},
		{ID: "stray", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: all, TokenEnv: "HEARSAY_TEST_STRAY_TOKEN", Identities: ids("stray")},
		{ID: "peek", Kind: principal.KindAgent, Class: principal.ClassObserver, Grant: all, TokenEnv: "HEARSAY_TEST_PEEK_TOKEN", Identities: ids("peek")},
	}
	sources := []connector.SourceConfig{
		{ID: w.src, Type: conversation.Type, Containers: []string{"kyle"}, Settings: json.RawMessage(`{"owner":"kyle","agents":["kern"]}`)},
		{ID: w.other, Type: conversation.Type, Containers: []string{"sam"}, Settings: json.RawMessage(`{"owner":"sam","agents":["kern","stray"]}`)},
	}
	store := l0.New(pool)
	for _, src := range sources {
		c, err := conversation.New(src, principals, func(name string) (string, bool) {
			v := map[string]string{"HEARSAY_TEST_KERN_TOKEN": tokens["kern"], "HEARSAY_TEST_STRAY_TOKEN": tokens["stray"]}[name]
			return v, v != ""
		})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(c.Handler(connector.NewGate(store, src.ID, c.Describe(), connector.NewAllowlist(src))))
		t.Cleanup(srv.Close)
		w.hooks[src.ID] = srv
	}
	calls, err := api.NewCalls(pool, config.Repo{Sources: sources, Principals: principals}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := api.Handler(calls, pool)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		lose := w.withhold != nil && w.withhold(r)
		w.mu.Unlock()
		if lose {
			inner.ServeHTTP(httptest.NewRecorder(), r)
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(rw, r)
	}))
	t.Cleanup(server.Close)
	w.url = server.URL
	return w
}

func (w *deleteWorld) message(t *testing.T, source, owner, exchange, role, text string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"owner": owner, "conversation": "c-1", "exchange": exchange, "role": role,
		"time": "2026-11-02T09:00:00Z", "text": text})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.hooks[source].URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.tokens["kern"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	var accepted struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(out, &accepted)
	return resp.StatusCode, accepted.EventID
}

func (w *deleteWorld) stored(t *testing.T, source, owner, exchange, role, text string) string {
	t.Helper()
	status, id := w.message(t, source, owner, exchange, role, text)
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("POST %s/%s = %d, want 202", exchange, role, status)
	}
	return id
}

func (w *deleteWorld) send(t *testing.T, mcp bool, c who, args any) (int, []byte) {
	t.Helper()
	status, body, err := invoke(t, w.url, w.tokens, mcp, c, "delete", args)
	if err != nil {
		return -2, nil
	}
	return status, body
}

func (w *deleteWorld) deleted(t *testing.T, mcp bool, c who, args map[string]any) api.Deleted {
	t.Helper()
	status, body := w.send(t, mcp, c, args)
	if status != http.StatusOK {
		t.Fatalf("delete %v = %d %s", args, status, body)
	}
	var out api.Deleted
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("delete answered %s: %v", body, err)
	}
	return out
}

func (w *deleteWorld) refusal(t *testing.T, mcp bool, c who, args map[string]any) (code string, status int, body []byte) {
	t.Helper()
	status, body = w.send(t, mcp, c, args)
	var out refused
	if err := json.Unmarshal(body, &out); err != nil || out.Code == "" || out.Error == "" {
		t.Fatalf("delete %v = %d %s, want a refusal with a code and a message", args, status, body)
	}
	return out.Code, status, body
}

func (w *deleteWorld) counts(t *testing.T) (records, events int) {
	t.Helper()
	if err := w.pool.QueryRow(t.Context(), `
SELECT (SELECT count(*) FROM l0_deletions d WHERE d.events && (SELECT coalesce(array_agg(id), '{}') FROM l0_events WHERE source = ANY($1))),
       (SELECT count(*) FROM l0_events e JOIN l0_deletions d ON d.retraction = e.id
          WHERE e.kind = 'deletion' AND d.events && (SELECT coalesce(array_agg(id), '{}') FROM l0_events WHERE source = ANY($1)))`,
		[]string{w.src, w.other}).Scan(&records, &events); err != nil {
		t.Fatal(err)
	}
	return records, events
}

func (w *deleteWorld) readable(t *testing.T, source, artifact string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events WHERE source = $1 AND artifact = $2 AND deletion IS NULL`, source, artifact).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *deleteWorld) document(t *testing.T, name string, refs ...string) string {
	t.Helper()
	id := "l1:" + w.src + ":" + name
	if _, err := w.pool.Exec(t.Context(), `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,artifact_class)
VALUES($1,'chat_thread',$2,$3,$4,now(),now(),now(),'[{"kind":"public"}]','text','raw','{}','none','chat_thread')`, id, w.src, name, refs); err != nil {
		t.Fatal(err)
	}
	return id
}

func (w *deleteWorld) queued(t *testing.T, doc string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE kind = 'distill' AND target_id = $1`, doc).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var (
	kyleAlone    = who{human: "kyle"}
	kyleWithKern = who{human: "kyle", agent: "kern"}
)

func TestDeleteRedactsEveryRevisionOfEachNamedMessage(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mcp     bool
		caller  who
		carrier string
	}{
		{"the owner over HTTP", false, kyleAlone, ""},
		{"the owner over MCP", true, kyleAlone, ""},
		{"the owner with the agent over HTTP", false, kyleWithKern, "kern"},
		{"the owner with the agent over MCP", true, kyleWithKern, "kern"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newDeleteWorld(t)
			first := w.stored(t, w.src, "kyle", "x-1", "owner", "The passphrase is tangerine-otter-42.")
			revised := w.stored(t, w.src, "kyle", "x-1", "owner", "The passphrase is a secret.")
			answer := w.stored(t, w.src, "kyle", "x-1", "assistant", "I will not repeat it.")
			untouched := w.stored(t, w.src, "kyle", "x-2", "owner", "A different exchange.")
			doc := w.document(t, "talk", first, answer, untouched)

			got := w.deleted(t, tt.mcp, tt.caller, map[string]any{"event_ids": []string{first, answer}, "idempotency_id": w.src + "-forget-1"})
			want := []string{first, revised, answer}
			slices.Sort(want)
			if !slices.Equal(got.Deleted, want) || !slices.Equal(got.Kept, []string{"ids", "times", "kinds"}) || !strings.HasPrefix(got.DeletionID, "del_") {
				t.Errorf("delete = %+v, want every revision of both messages and what is kept", got)
			}
			if n := w.readable(t, w.src, "c-1/x-1:owner") + w.readable(t, w.src, "c-1/x-1:assistant"); n != 0 {
				t.Errorf("%d revisions of the named messages are still readable", n)
			}
			if w.readable(t, w.src, "c-1/x-2:owner") != 1 {
				t.Error("another exchange's message was deleted")
			}
			if records, events := w.counts(t); records != 1 || events != 1 {
				t.Errorf("%d deletion records and %d deletion events, want one of each", records, events)
			}
			if w.queued(t, doc) != 1 {
				t.Errorf("the document was queued %d times, want once", w.queued(t, doc))
			}
			var carrier string
			if err := w.pool.QueryRow(t.Context(), `SELECT coalesce(selector->'Call'->>'carrier', '') FROM l0_deletions WHERE id = $1`, got.DeletionID).Scan(&carrier); err != nil {
				t.Fatal(err)
			}
			if carrier != tt.carrier {
				t.Errorf("the deletion records carrier %q, want %q", carrier, tt.carrier)
			}

			for _, text := range []string{"The passphrase is tangerine-otter-42.", "Something new."} {
				if status, _ := w.message(t, w.src, "kyle", "x-1", "owner", text); status != http.StatusGone {
					t.Errorf("a post of %q = %d, want 410", text, status)
				}
			}
		})
	}
}

func TestDeleteRefusesTheWholeRequestWithOneNotFound(t *testing.T) {
	w := newDeleteWorld(t)
	own := w.stored(t, w.src, "kyle", "x-1", "owner", "Mine.")
	sams := w.stored(t, w.other, "sam", "x-1", "owner", "Sam's.")
	doc := w.document(t, "talk", own)
	nonexistent := connector.EventID(w.src, "c-9/x-9:owner@0000000000000000")

	foreign := connector.Event{Source: w.src + "z", NativeID: "m1", Kind: connector.KindMessage, Time: day,
		Payload: connector.Payload{Artifact: "m1", Text: "chat", Author: &connector.Identity{Source: w.src + "z", Kind: connector.IdentityUser, NativeID: "u1"}, Container: connector.Container{Kind: connector.ContainerChannel, NativeID: "c"}},
		ACL:     connector.ACL{{Kind: connector.ACLPublic}}}
	if _, err := l0.New(w.pool).Append(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	foreignID := connector.EventID(foreign.Source, foreign.NativeID)

	var first []byte
	for _, tt := range []struct {
		name   string
		caller who
		ids    []string
	}{
		{"another person's conversation", kyleAlone, []string{own, sams}},
		{"a nonexistent id", kyleAlone, []string{own, nonexistent}},
		{"a source of another type", kyleAlone, []string{own, foreignID}},
		{"a person who owns no conversation", who{human: "sam"}, []string{own}},
		{"an agent the source does not allow", who{human: "sam", agent: "stray"}, []string{own}},
		{"an agent the owner's source does not allow", who{human: "kyle", agent: "stray"}, []string{own}},
	} {
		for _, mcp := range []bool{false, true} {
			code, status, body := w.refusal(t, mcp, tt.caller, map[string]any{"event_ids": tt.ids, "idempotency_id": w.src + "-refused"})
			if code != "not_found" || (!mcp && status != http.StatusNotFound) {
				t.Errorf("%s (mcp %v) = %d %s, want 404 not_found", tt.name, mcp, status, body)
			}
			if first == nil {
				first = body
			} else if !bytes.Equal(first, body) {
				t.Errorf("%s (mcp %v) answered %s, want the same bytes as %s", tt.name, mcp, body, first)
			}
		}
	}
	if code, _, _ := w.refusal(t, false, who{human: "kyle", agent: "peek"}, map[string]any{"event_ids": []string{own}, "idempotency_id": w.src + "-peek"}); code != "not_allowed" {
		t.Errorf("an observer carrying = %s, want not_allowed", code)
	}
	if w.readable(t, w.src, "c-1/x-1:owner") != 1 || w.readable(t, w.other, "c-1/x-1:owner") != 1 {
		t.Error("a refused request deleted a message")
	}
	if records, events := w.counts(t); records != 0 || events != 0 {
		t.Errorf("%d deletion records and %d deletion events after refusals, want none", records, events)
	}
	if w.queued(t, doc) != 0 {
		t.Error("a refused request queued a document")
	}
}

func TestDeleteRefusesAMalformedRequest(t *testing.T) {
	w := newDeleteWorld(t)
	id := w.stored(t, w.src, "kyle", "x-1", "owner", "Mine.")
	many := make([]string, 101)
	for i := range many {
		many[i] = id
	}
	for _, tt := range []struct {
		name string
		args map[string]any
	}{
		{"no events", map[string]any{"event_ids": []string{}, "idempotency_id": "a"}},
		{"too many events", map[string]any{"event_ids": many, "idempotency_id": "a"}},
		{"no idempotency id", map[string]any{"event_ids": []string{id}}},
		{"a blank idempotency id", map[string]any{"event_ids": []string{id}, "idempotency_id": "  "}},
		{"an idempotency id that is too long", map[string]any{"event_ids": []string{id}, "idempotency_id": strings.Repeat("a", 257)}},
		{"a blank event id", map[string]any{"event_ids": []string{""}, "idempotency_id": "a"}},
		{"an unknown field", map[string]any{"event_ids": []string{id}, "idempotency_id": "a", "reason": "why"}},
	} {
		for _, mcp := range []bool{false, true} {
			if code, _, body := w.refusal(t, mcp, kyleAlone, tt.args); code != "invalid_request" {
				t.Errorf("%s (mcp %v) = %s, want invalid_request", tt.name, mcp, body)
			}
		}
	}
	if w.readable(t, w.src, "c-1/x-1:owner") != 1 {
		t.Error("a refused request deleted a message")
	}
}

func TestDeleteIsIdempotentOnTheIdempotencyID(t *testing.T) {
	w := newDeleteWorld(t)
	a := w.stored(t, w.src, "kyle", "x-1", "owner", "Mine.")
	b := w.stored(t, w.src, "kyle", "x-1", "assistant", "Yours.")
	idem := w.src + "-forget"
	args := map[string]any{"event_ids": []string{a, b}, "idempotency_id": idem}

	var lost sync.Once
	w.withhold = func(r *http.Request) bool {
		drop := false
		if r.URL.Path == "/v1/delete" {
			lost.Do(func() { drop = true })
		}
		return drop
	}
	if status, _ := w.send(t, false, kyleWithKern, args); status != -2 {
		t.Fatalf("the first request = %d, want its answer lost", status)
	}
	if records, _ := w.counts(t); records != 1 {
		t.Fatalf("%d deletion records after the lost answer, want the one it committed", records)
	}
	status, original := w.send(t, false, kyleWithKern, args)
	if status != http.StatusOK {
		t.Fatalf("the retry = %d %s", status, original)
	}

	const n = 8
	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s int
			s, bodies[i] = w.send(t, i%2 == 0, kyleWithKern, args)
			if s != http.StatusOK {
				t.Errorf("request %d = %d %s", i, s, bodies[i])
			}
		}()
	}
	wg.Wait()
	for i, body := range bodies {
		if !bytes.Equal(body, original) {
			t.Errorf("request %d answered %s, want the original %s", i, body, original)
		}
	}
	if records, events := w.counts(t); records != 1 || events != 1 {
		t.Errorf("%d deletion records and %d deletion events, want one of each", records, events)
	}

	if code, status, body := w.refusal(t, false, kyleWithKern, map[string]any{"event_ids": []string{a}, "idempotency_id": idem}); code != "conflict" || status != http.StatusConflict {
		t.Errorf("the id reused for other events = %d %s, want 409 conflict", status, body)
	}
}

func TestConcurrentFirstRequestsRecordOneDeletion(t *testing.T) {
	w := newDeleteWorld(t)
	var ids []string
	for _, x := range []string{"x-1", "x-2", "x-3"} {
		ids = append(ids, w.stored(t, w.src, "kyle", x, "owner", "Words of "+x))
	}
	const n = 12
	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s int
			s, bodies[i] = w.send(t, i%2 == 0, kyleWithKern, map[string]any{"event_ids": ids, "idempotency_id": w.src + "-race"})
			if s != http.StatusOK {
				t.Errorf("request %d = %d %s", i, s, bodies[i])
			}
		}()
	}
	wg.Wait()
	for i, body := range bodies {
		if !bytes.Equal(body, bodies[0]) {
			t.Errorf("request %d answered %s, want %s", i, body, bodies[0])
		}
	}
	if records, events := w.counts(t); records != 1 || events != 1 {
		t.Errorf("%d deletion records and %d deletion events, want one of each", records, events)
	}
}

func TestDeleteOfWhatIsAlreadyDeletedRecordsNothingMore(t *testing.T) {
	w := newDeleteWorld(t)
	id := w.stored(t, w.src, "kyle", "x-1", "owner", "Mine.")
	first := w.deleted(t, false, kyleAlone, map[string]any{"event_ids": []string{id}, "idempotency_id": w.src + "-one"})
	again := w.deleted(t, false, kyleAlone, map[string]any{"event_ids": []string{id}, "idempotency_id": w.src + "-two"})
	if again.DeletionID != first.DeletionID || len(again.Deleted) != 0 {
		t.Errorf("a second deletion = %+v, want nothing deleted, under the first's record %s", again, first.DeletionID)
	}
	if records, events := w.counts(t); records != 1 || events != 1 {
		t.Errorf("%d deletion records and %d deletion events, want one of each", records, events)
	}
}

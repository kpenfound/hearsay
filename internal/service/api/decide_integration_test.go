//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/config/configtest"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

type decideWorld struct {
	*world
	graph                  *l2.Store
	topic                  l2.Topic
	first, second, private l2.Stance
	docFirst, docSecond    string
	docPrivate             string
	repo                   config.Repo
	url                    string
	tokens                 map[string]string
	append                 func(doc, position string, hour int) l2.Stance
}

const decideScopeAuthority = "scope: \"*\"\nratified_by:\n  principals: [kyle]\n"

func loadAuthority(t *testing.T, policy string) config.Authority {
	t.Helper()
	files := map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/eng.yaml":     "id: eng\nsources: [github]\n",
		"principals/p.yaml":   "- id: kyle\n  identities: [{source: github, native_id: u1}]\n",
		"authority/a.yaml":    policy,
	}
	dir := configtest.Write(t, files)
	repo, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	return repo.Authority
}

func newDecideWorld(t *testing.T, authority config.Authority) *decideWorld {
	t.Helper()
	w := newWorld(t)
	t.Setenv("HEARSAY_TEST_BOSS_TOKEN", "test-boss-api-credential")
	t.Setenv("HEARSAY_TEST_ANA_TOKEN", "test-ana-api-credential")
	d := &decideWorld{world: w, graph: l2.New(w.pool), tokens: map[string]string{
		"kyle": "test-kyle-api-credential", "sam": "test-sam-api-credential", "shed": "test-shed-api-credential",
		"peek": "test-peek-api-credential", "boss": "test-boss-api-credential", "ana": "test-ana-api-credential",
	}}
	d.repo = repo(w.src)
	d.repo.Principals = append(d.repo.Principals,
		principal.Principal{ID: "boss", Kind: principal.KindAgent, Class: principal.ClassSteward, Grant: principal.Grant{Scopes: principal.AllScopes()}, TokenEnv: "HEARSAY_TEST_BOSS_TOKEN"},
		principal.Principal{ID: "ana", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes("tracker:elsewhere")}, TokenEnv: "HEARSAY_TEST_ANA_TOKEN"})
	d.repo.Authority = authority
	calls, err := api.NewCalls(w.pool, d.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler(calls, w.pool))
	t.Cleanup(server.Close)
	d.url, d.world.calls = server.URL, calls

	ctx := t.Context()
	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode, Label: "kpenfound"}}
	d.docFirst = w.putIn(w.scope, w.project+"#40", l1.KindIssue, 10, "The platform team runs the migration.", public)
	d.docSecond = w.putIn(w.scope, w.project+"#41", l1.KindIssue, 11, "Each service team runs its migration.", public)
	d.docPrivate = w.putIn(w.scope, w.project+"#42", l1.KindIssue, 12, "Legal runs it.", private)
	d.topic = l2.Topic{ID: l2.TopicID(w.src, d.docFirst, 0, "who runs the migration"), Scope: w.src, Name: "who runs the migration",
		About: []string{w.scope}, ACL: public, OpenedBy: d.docFirst}
	if _, err := d.graph.OpenTopic(ctx, d.topic); err != nil {
		t.Fatal(err)
	}
	d.append = func(doc, position string, hour int) l2.Stance {
		t.Helper()
		acl := public
		if doc == d.docPrivate {
			acl = private
		}
		at := day.Add(time.Duration(hour) * time.Hour)
		st, _, err := d.graph.AppendStance(ctx, l2.Stance{ID: l2.StanceID(d.topic.ID, doc, position, at, l2.TierInferred), TopicID: d.topic.ID,
			Position: position, Author: "kyle", StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: acl}, at)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	d.first = d.append(d.docFirst, "the platform team", 10)
	d.second = d.append(d.docSecond, "each service team", 11)
	d.private = d.append(d.docPrivate, "legal", 9)
	return d
}

func (d *decideWorld) send(t *testing.T, mcp bool, c who, args any) (int, []byte) {
	t.Helper()
	if m, ok := args.(map[string]any); ok {
		if id, ok := m["decision_id"].(string); ok {
			scoped := map[string]any{}
			for k, v := range m {
				scoped[k] = v
			}
			scoped["decision_id"] = d.src + "-" + id
			args = scoped
		}
	}
	status, body, err := invoke(t, d.url, d.tokens, mcp, c, "decide", args)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

func (d *decideWorld) decided(t *testing.T, mcp bool, c who, args map[string]any) api.Decided {
	t.Helper()
	status, body := d.send(t, mcp, c, args)
	if status != http.StatusOK {
		t.Fatalf("decide %v = %d %s", args, status, body)
	}
	var out api.Decided
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decide answered %s: %v", body, err)
	}
	return out
}

type refused struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (d *decideWorld) refusal(t *testing.T, mcp bool, c who, args map[string]any) (int, refused, []byte) {
	t.Helper()
	status, body := d.send(t, mcp, c, args)
	var out refused
	if err := json.Unmarshal(body, &out); err != nil || out.Code == "" || out.Error == "" {
		t.Fatalf("decide %v = %d %s, want a refusal with a code and a message", args, status, body)
	}
	return status, out, body
}

func (d *decideWorld) ledger(t *testing.T) []l2.Gesture {
	t.Helper()
	all, err := d.graph.Gestures(t.Context(), d.topic.Scope)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

var (
	kyleOnly    = who{human: "kyle"}
	kyleViaShed = who{human: "kyle", agent: "shed"}
	samViaBoss  = who{human: "sam", agent: "boss"}
	kyleViaPeek = who{human: "kyle", agent: "peek"}
	ratifyArgs  = func(id, stance string) map[string]any {
		return map[string]any{"decision_id": id, "action": "ratify", "stance_id": stance}
	}
	demoteArgs = func(id, stance string) map[string]any {
		return map[string]any{"decision_id": id, "action": "demote", "stance_id": stance}
	}
	undoArgs = func(id string, gesture string) map[string]any {
		return map[string]any{"decision_id": id, "action": "undo", "gesture_id": gesture}
	}
	idsInAnAnswer = regexp.MustCompile(`"(gesture_id|stance_id|undone_gesture_id)":"[^"]*"`)
)

func (d *decideWorld) standing(t *testing.T) standing {
	t.Helper()
	return d.world.standingOf(t, d.world.calls, d.repo.Authority, d.topic)
}

func (d *decideWorld) traverseTier(t *testing.T) string {
	t.Helper()
	body, err := d.world.calls.Call(t.Context(), kyle, "traverse", mustJSON(t, map[string]any{
		"start": []map[string]string{{"type": "topic", "id": d.topic.ID}}, "steps": []map[string]any{{"edge": "stances"}}}))
	if err != nil {
		t.Fatalf("traverse = %v", err)
	}
	var g struct {
		Nodes []struct{ ID, Tier, Position string } `json:"nodes"`
	}
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if n.ID == d.topic.ID {
			return n.Tier
		}
	}
	t.Fatalf("traverse did not serve the topic: %s", body)
	return ""
}

func TestDecideAnswersTheSameBytesOverHTTPAndMCP(t *testing.T) {
	mask := func(b []byte) string { return idsInAnAnswer.ReplaceAllString(string(b), `"$1":"-"`) }
	httpWorld := newDecideWorld(t, config.Authority{})
	mcpWorld := newDecideWorld(t, config.Authority{})
	httpArgs := ratifyArgs("http", httpWorld.second.ID)
	mcpArgs := ratifyArgs("mcp", mcpWorld.second.ID)
	args := func(d *decideWorld) map[string]any {
		if d == httpWorld {
			return httpArgs
		}
		return mcpArgs
	}
	hs, hb := httpWorld.send(t, false, kyleViaShed, args(httpWorld))
	ms, mb := mcpWorld.send(t, true, kyleViaShed, args(mcpWorld))
	if hs != http.StatusOK || ms != http.StatusOK {
		t.Fatalf("first decisions = %d %s, %d %s", hs, hb, ms, mb)
	}
	if mask(hb) != mask(mb) || !strings.Contains(string(hb), `"recorded":true`) {
		t.Errorf("first decision: HTTP %s, MCP %s; want the same, both recorded", hb, mb)
	}

	_, over := httpWorld.send(t, false, kyleViaShed, args(httpWorld))
	_, then := httpWorld.send(t, true, kyleViaShed, args(httpWorld))
	if string(over) != string(then) || !strings.Contains(string(over), `"recorded":false`) {
		t.Errorf("replay: HTTP %s, MCP %s; want the same bytes, not recorded", over, then)
	}
	var first, again api.Decided
	_ = json.Unmarshal(hb, &first)
	_ = json.Unmarshal(over, &again)
	if first.GestureID != again.GestureID || first.GestureID == "" {
		t.Errorf("the replay answered gesture %q, want the original %q", again.GestureID, first.GestureID)
	}

	_, a, rb := httpWorld.refusal(t, false, kyleViaShed, ratifyArgs("x", "stance:none"))
	_, _, mrb := httpWorld.refusal(t, true, kyleViaShed, ratifyArgs("x", "stance:none"))
	if string(rb) != string(mrb) || a.Code != "not_found" {
		t.Errorf("refusal: HTTP %s, MCP %s", rb, mrb)
	}
}

func TestDecideChangesWhatEveryReadServes(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	ctx := t.Context()
	want := func(step, tier string) {
		t.Helper()
		got := d.standing(t)
		position := "each service team"
		line := position + " @ " + tier
		if got != (standing{line, line, line}) {
			t.Errorf("%s: bundle %q, L3 %q, history %q; want %q from all three", step, got.bundle, got.view, got.history, line)
		}
		if tt := d.traverseTier(t); tt != tier {
			t.Errorf("%s: traverse serves tier %q, want %q", step, tt, tier)
		}
	}
	want("before any decision", "inferred")
	ratify := d.decided(t, false, kyleOnly, ratifyArgs("r1", d.second.ID))
	want("ratified over HTTP", "ratified")
	undone := d.decided(t, true, kyleViaShed, undoArgs("u1", ratify.GestureID))
	want("the ratify undone over MCP", "inferred")
	if undone.UndoneGestureID != ratify.GestureID || undone.Action != "undo" {
		t.Errorf("the undo answered %+v", undone)
	}
	demote := d.decided(t, true, kyleViaShed, demoteArgs("d1", d.second.ID))
	want("demoted over MCP", "contested")
	d.decided(t, false, kyleOnly, undoArgs("u2", demote.GestureID))
	want("the demote undone", "inferred")

	for _, other := range []struct{ name, event string }{
		{"a reaction", connector.EventID("discord", "reaction:"+dbtest.UniqueID())},
		{"the CLI", connector.EventID("hearsay", "cli-gesture-"+dbtest.UniqueID())},
	} {
		g, _, err := l2.RecordGesture(ctx, d.pool, d.repo, l2.GestureRequest{Event: other.event, Principal: "sam", Action: l2.GestureDemote, Documents: []string{d.docSecond}})
		if err != nil {
			t.Fatal(err)
		}
		want("demoted by "+other.name, "contested")
		d.decided(t, false, kyleOnly, undoArgs("undo-"+other.event, strconv.FormatInt(g.ID, 10)))
		want("the demotion by "+other.name+" undone through decide", "inferred")
	}

	d.decided(t, false, kyleOnly, ratifyArgs("r-first", d.first.ID))
	want("another stance ratified", "inferred")
}

func TestDecideRecordsThePrincipalAndTheCarrier(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	carried := d.decided(t, true, kyleViaShed, ratifyArgs("carried", d.second.ID))
	alone := d.decided(t, false, who{human: "sam"}, demoteArgs("alone", d.first.ID))
	for _, g := range d.ledger(t) {
		switch strconv.FormatInt(g.ID, 10) {
		case carried.GestureID:
			if g.Principal != "kyle" || g.Carrier != "shed" || g.Stance != d.second.ID || g.Action != l2.GestureRatify {
				t.Errorf("the carried decision = %+v, want kyle's, carried by shed", g)
			}
		case alone.GestureID:
			if g.Principal != "sam" || g.Carrier != "" {
				t.Errorf("the decision with no agent = %+v, want sam's with no carrier", g)
			}
		}
	}
}

func TestDecideChecksTheScopePolicyAgainstTheHumanAndTheCarrierClass(t *testing.T) {
	d := newDecideWorld(t, loadAuthority(t, decideScopeAuthority))
	before := len(d.ledger(t))
	for _, tt := range []struct {
		name string
		c    who
	}{
		{"a human the policy does not allow, with no agent", who{human: "sam"}},
		{"a human the policy does not allow, with a steward", samViaBoss},
		{"an allowed human, with an observer", kyleViaPeek},
	} {
		for _, mcp := range []bool{false, true} {
			status, got, body := d.refusal(t, mcp, tt.c, ratifyArgs("denied-"+tt.name, d.second.ID))
			if got.Code != "not_allowed" || (!mcp && status != http.StatusForbidden) {
				t.Errorf("%s (mcp=%v) = %d %s, want not_allowed", tt.name, mcp, status, body)
			}
		}
	}
	if after := len(d.ledger(t)); after != before {
		t.Errorf("%d gestures after refusals, want the %d there were", after, before)
	}
	if g := d.decided(t, true, kyleViaShed, ratifyArgs("allowed", d.second.ID)); !g.Recorded {
		t.Errorf("an allowed human through a worker = %+v, want recorded", g)
	}
}

func TestDecideIsIdempotentOnTheDecisionID(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	first := d.decided(t, false, kyleViaShed, ratifyArgs("once", d.second.ID))
	if !first.Recorded {
		t.Fatalf("the first answer = %+v, want recorded", first)
	}

	d.append(d.docSecond, "each service team, behind a flag", 20)
	for _, mcp := range []bool{false, true} {
		again := d.decided(t, mcp, kyleViaShed, ratifyArgs("once", d.second.ID))
		if again.Recorded || again.GestureID != first.GestureID {
			t.Errorf("the replay after the stance was superseded (mcp=%v) = %+v, want gesture %s, not recorded", mcp, again, first.GestureID)
		}
	}

	if _, got, _ := d.refusal(t, false, kyleViaShed, ratifyArgs("another", d.second.ID)); got.Code != "not_live" {
		t.Errorf("a new ratify of a superseded stance = %+v, want not_live", got)
	}

	live := d.first
	r := d.decided(t, false, kyleOnly, demoteArgs("demote-live", live.ID))
	undo := d.decided(t, false, kyleOnly, undoArgs("undo-once", r.GestureID))
	retry := d.decided(t, true, kyleOnly, undoArgs("undo-once", r.GestureID))
	if retry.Recorded || retry.GestureID != undo.GestureID {
		t.Errorf("a retried undo = %+v, want gesture %s, not recorded", retry, undo.GestureID)
	}

	if _, got, _ := d.refusal(t, false, kyleOnly, undoArgs("undo-twice", r.GestureID)); got.Code != "not_undoable" {
		t.Errorf("a new undo of an undone gesture = %+v, want not_undoable", got)
	}

	before := len(d.ledger(t))
	for _, args := range []map[string]any{demoteArgs("once", d.second.ID), ratifyArgs("once", d.first.ID)} {
		if status, got, _ := d.refusal(t, false, kyleViaShed, args); got.Code != "conflict" || status != http.StatusConflict {
			t.Errorf("%v = %d %+v, want conflict", args, status, got)
		}
	}

	if g := d.decided(t, false, kyleOnly, ratifyArgs("once", d.first.ID)); !g.Recorded {
		t.Errorf("the same id with no carrier = %+v, want a new decision", g)
	}
	if got := len(d.ledger(t)); got != before+1 {
		t.Errorf("%d gestures, want %d: only the decision with no carrier is new", got, before+1)
	}

	k := d.decided(t, false, kyleOnly, ratifyArgs("shared", d.first.ID))
	s := d.decided(t, false, who{human: "sam"}, ratifyArgs("shared", d.first.ID))
	if !k.Recorded || !s.Recorded || k.GestureID == s.GestureID {
		t.Errorf("two humans, one id: %+v and %+v, want two separate decisions", k, s)
	}
}

func TestConcurrentIdenticalDecisionsRecordOneGesture(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	const n = 8
	var wg sync.WaitGroup
	answers := make([]api.Decided, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := d.send(t, i%2 == 0, kyleViaShed, ratifyArgs("race", d.second.ID))
			if status != http.StatusOK {
				t.Errorf("request %d = %d %s", i, status, body)
				return
			}
			_ = json.Unmarshal(body, &answers[i])
		}()
	}
	wg.Wait()
	recorded := 0
	for _, a := range answers {
		if a.Recorded {
			recorded++
		}
		if a.GestureID != answers[0].GestureID {
			t.Errorf("answers name gestures %s and %s", a.GestureID, answers[0].GestureID)
		}
	}
	if recorded != 1 {
		t.Errorf("%d answers said recorded, want exactly one", recorded)
	}
}

func TestDecideRefusesWithOneAnswerForWhatTheCallerMayNotRead(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	sam := who{human: "sam"}
	ana := who{human: "ana"}

	hidden := d.decided(t, false, kyleOnly, ratifyArgs("hidden", d.private.ID))

	mine := d.decided(t, false, sam, ratifyArgs("mine", d.first.ID))
	mineUndo := d.decided(t, false, sam, undoArgs("mine-undo", mine.GestureID))

	type probe struct {
		name string
		c    who
		args map[string]any
	}
	probes := []probe{
		{"a stance that does not exist", sam, ratifyArgs("p1", "stance:nobody")},
		{"a stance sam may not read", sam, ratifyArgs("p2", d.private.ID)},
		{"a stance out of reach", ana, ratifyArgs("p3", d.first.ID)},
		{"a gesture that does not exist", sam, undoArgs("p4", "999999999")},
		{"a gesture sam may not read", sam, undoArgs("p5", hidden.GestureID)},
		{"a gesture out of reach", ana, undoArgs("p6", mine.GestureID)},
		{"a demote of a stance sam may not read", sam, demoteArgs("p7", d.private.ID)},
	}
	var wantStatus int
	var wantBody string
	for i, p := range probes {
		for _, mcp := range []bool{false, true} {
			status, got, body := d.refusal(t, mcp, p.c, p.args)
			if got.Code != "not_found" {
				t.Errorf("%s (mcp=%v) = %d %s, want not_found", p.name, mcp, status, body)
			}
			if i == 0 && !mcp {
				wantStatus, wantBody = status, string(body)
			}
			if !mcp && (status != wantStatus || string(body) != wantBody) {
				t.Errorf("%s = %d %s, want the same as a stance that does not exist: %d %s", p.name, status, body, wantStatus, wantBody)
			}
			if mcp && string(body) != wantBody {
				t.Errorf("%s over MCP = %s, want %s", p.name, body, wantBody)
			}
		}
	}

	private := connector.ACL{{Kind: connector.ACLIdentity, Source: d.src, NativeID: kyleNode, Label: "kpenfound"}}
	doc, err := l1.New(d.pool).Get(t.Context(), d.docFirst)
	if err != nil {
		t.Fatal(err)
	}
	doc.ACL = private
	if _, err := l1.New(d.pool).Put(t.Context(), doc.Document); err != nil {
		t.Fatal(err)
	}
	for _, mcp := range []bool{false, true} {
		status, got, body := d.refusal(t, mcp, sam, ratifyArgs("mine", d.first.ID))
		if got.Code != "not_found" || string(body) != wantBody || (!mcp && status != wantStatus) {
			t.Errorf("a ratify replay after access was lost (mcp=%v) = %d %s, want %d %s", mcp, status, body, wantStatus, wantBody)
		}
		_, got, body = d.refusal(t, mcp, sam, undoArgs("mine-undo", mine.GestureID))
		if got.Code != "not_found" || string(body) != wantBody {
			t.Errorf("an undo replay after access was lost (mcp=%v) = %s, want %s (gesture %s)", mcp, body, wantBody, mineUndo.GestureID)
		}
	}

	if g := d.decided(t, false, kyleOnly, ratifyArgs("hidden", d.private.ID)); g.Recorded || g.GestureID != hidden.GestureID {
		t.Errorf("kyle's replay = %+v", g)
	}
}

func TestDecideRefusesWhatCannotBeDecided(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	ctx := t.Context()
	withdrawn := d.append(d.docPrivate, "legal, again", 13)
	if _, err := d.pool.Exec(ctx, `UPDATE l2_stances SET withdrawn = true WHERE id = $1`, withdrawn.ID); err != nil {
		t.Fatal(err)
	}
	old := d.second
	d.append(d.docSecond, "each service team, by quarter", 14)
	pin, _, err := l2.RecordGesture(ctx, d.pool, d.repo, l2.GestureRequest{Event: connector.EventID("hearsay", "cli-gesture-"+dbtest.UniqueID()), Principal: "kyle", Action: l2.GesturePin, Documents: []string{d.docFirst}})
	if err != nil {
		t.Fatal(err)
	}
	ratified := d.decided(t, false, kyleOnly, ratifyArgs("base", d.first.ID))
	undo := d.decided(t, false, kyleOnly, undoArgs("base-undo", ratified.GestureID))
	before := len(d.ledger(t))

	for _, tt := range []struct {
		name string
		args map[string]any
		code string
		http int
	}{
		{"a superseded stance", ratifyArgs("n1", old.ID), "not_live", http.StatusConflict},
		{"a superseded stance, demoted", demoteArgs("n2", old.ID), "not_live", http.StatusConflict},
		{"a withdrawn stance", ratifyArgs("n3", withdrawn.ID), "not_live", http.StatusConflict},
		{"an undone gesture", undoArgs("n4", ratified.GestureID), "not_undoable", http.StatusConflict},
		{"an undo", undoArgs("n5", undo.GestureID), "not_undoable", http.StatusConflict},
		{"a pin", undoArgs("n6", strconv.FormatInt(pin.ID, 10)), "not_undoable", http.StatusConflict},
		{"a pin action", map[string]any{"decision_id": "n7", "action": "pin", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"an unpin action", map[string]any{"decision_id": "n8", "action": "unpin", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"a merge action", map[string]any{"decision_id": "n9", "action": "merge", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"a split action", map[string]any{"decision_id": "n10", "action": "split", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"an alias action", map[string]any{"decision_id": "n11", "action": "confirm_alias", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"no action", map[string]any{"decision_id": "n12", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"no decision id", map[string]any{"action": "ratify", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"no stance", map[string]any{"decision_id": "n13", "action": "ratify"}, "invalid", http.StatusBadRequest},
		{"a gesture on a ratify", map[string]any{"decision_id": "n14", "action": "ratify", "stance_id": d.first.ID, "gesture_id": "1"}, "invalid", http.StatusBadRequest},
		{"a stance on an undo", map[string]any{"decision_id": "n15", "action": "undo", "stance_id": d.first.ID}, "invalid", http.StatusBadRequest},
		{"a gesture id that is no number", undoArgs("n16", "latest"), "invalid", http.StatusBadRequest},
		{"an unknown field", map[string]any{"decision_id": "n17", "action": "ratify", "stance_id": d.first.ID, "topic": "x"}, "invalid", http.StatusBadRequest},
	} {
		for _, mcp := range []bool{false, true} {
			status, got, body := d.refusal(t, mcp, kyleOnly, tt.args)
			if got.Code != tt.code || (!mcp && status != tt.http) {
				t.Errorf("%s (mcp=%v) = %d %s, want %s", tt.name, mcp, status, body, tt.code)
			}
		}
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/v1/decide", strings.NewReader(`{"decision_id":`))
	req.Header.Set(api.PrincipalHeader, "kyle")
	req.Header.Set(api.AuthorizationHeader, "Bearer test-kyle-api-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"code":"invalid"`) {
		t.Errorf("malformed JSON = %d %s, want invalid", resp.StatusCode, body)
	}
	if got := len(d.ledger(t)); got != before {
		t.Errorf("%d gestures after refusals, want the %d there were", got, before)
	}
}

func TestDecideCredentialFailuresAreTheSharedCodelessOnes(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	before := len(d.ledger(t))
	bad := str("not-the-credential")
	none := str("")
	for _, tt := range []struct {
		name string
		c    who
	}{
		{"no human token", who{human: "kyle", humanToken: none}},
		{"an invalid human token", who{human: "kyle", humanToken: bad}},
		{"an unnamed human", who{}},
		{"an unknown human", who{human: "nobody"}},
		{"an agent with no token", who{human: "kyle", agent: "shed", agentToken: none}},
		{"an agent with an invalid token", who{human: "kyle", agent: "shed", agentToken: bad}},
	} {
		for _, mcp := range []bool{false, true} {
			status, body := d.send(t, mcp, tt.c, ratifyArgs("cred", d.second.ID))

			other := tt.c
			otherBody, _ := json.Marshal(map[string]any{"id": d.docFirst})
			wantStatus, want := d.otherCall(t, mcp, other, "get_l1", otherBody)
			if status != wantStatus || string(body) != want || strings.Contains(string(body), `"code"`) {
				t.Errorf("%s (mcp=%v): decide = %d %s, get_l1 = %d %s; want the same status and a code-less body", tt.name, mcp, status, body, wantStatus, want)
			}
			if status == http.StatusOK || status == -1 {
				t.Errorf("%s (mcp=%v) = %d, want a refusal", tt.name, mcp, status)
			}
		}
	}
	if got := len(d.ledger(t)); got != before {
		t.Errorf("%d gestures after credential failures, want the %d there were", got, before)
	}

	_, body := d.otherCall(t, false, kyleOnly, "get_l1", []byte(`{"id":"l1:none:nothing"}`))
	if strings.Contains(string(body), `"code"`) {
		t.Errorf("get_l1 refusal = %s, want no code", body)
	}
}

func (d *decideWorld) otherCall(t *testing.T, mcp bool, c who, name string, args []byte) (int, string) {
	t.Helper()
	status, body, err := invoke(t, d.url, d.tokens, mcp, c, name, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return status, string(body)
}

func TestDecideReportsAnInternalFailureWithItsCode(t *testing.T) {
	d := newDecideWorld(t, config.Authority{})
	cases, err := api.NewCalls(txOnly{d.pool}, d.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cases.Call(context.Background(), api.Caller{Principal: "kyle"}, "decide", mustJSON(t, ratifyArgs("internal", d.second.ID)))
	var e *api.Error
	if !asAPIError(err, &e) || e.Code != "internal" || e.Status != http.StatusInternalServerError {
		t.Errorf("decide without a pool = %v, want an internal refusal with its code", err)
	}
}

type txOnly struct{ api.DB }

func asAPIError(err error, target **api.Error) bool { return errors.As(err, target) }

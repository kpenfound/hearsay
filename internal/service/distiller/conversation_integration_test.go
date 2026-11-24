//go:build integration

package distiller_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// The recorded conversations in testdata/conversations: the messages Kern
// posted, the segmentation and distill answers the model gave, and what Jev
// answered the outcome questions, by state. A state is the conversation at one
// moment: a message reworded, one that arrived, a topic the model no longer
// finds. The requests those answers were recorded against are not copied
// here; they are built from the same code the distiller runs, so a prompt that
// changes without the recording fails here and not in production.

type convMessage struct {
	Exchange    string    `json:"exchange"`
	Role        string    `json:"role"`
	At          time.Time `json:"at"`
	Text        string    `json:"text"`
	Interrupted bool      `json:"interrupted"`
}

type convState struct {
	Messages  []convMessage             `json:"messages"`
	Edit      map[string]string         `json:"edit"`
	Add       []convMessage             `json:"add"`
	Drop      []string                  `json:"drop"`
	Segments  []l1.MeetingSpan          `json:"segments"`
	Answers   map[string]map[string]any `json:"answers"`
	Judgments map[string]string         `json:"judgments"`
}

type convScenario struct {
	Conversation string               `json:"conversation"`
	Messages     []convMessage        `json:"messages"`
	States       map[string]convState `json:"states"`
}

// conversationOwner and conversationAgent are the principals the recordings
// were made for.
const (
	conversationOwner = "kyle"
	conversationAgent = "kern"
)

func loadScenario(t *testing.T, name string) convScenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "conversations", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var s convScenario
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("reading the %s recording: %v", name, err)
	}
	return s
}

// resolved is one state with its messages, answers and judgments settled.
type resolved struct {
	name     string
	thread   string
	messages []convMessage
	segments []l1.MeetingSpan
	answers  map[string]map[string]any
	judge    map[string]string
}

func (s convScenario) state(t *testing.T, name string) resolved {
	t.Helper()
	st, ok := s.States[name]
	if !ok {
		t.Fatalf("the recording has no state %q", name)
	}
	r := resolved{name: name, thread: s.Conversation, answers: map[string]map[string]any{}, judge: map[string]string{}}
	if base, ok := s.States["base"]; ok && name != "base" {
		maps.Copy(r.answers, base.Answers)
		maps.Copy(r.judge, base.Judgments)
	}
	maps.Copy(r.answers, st.Answers)
	maps.Copy(r.judge, st.Judgments)
	r.messages = slices.Clone(s.Messages)
	if st.Messages != nil {
		r.messages = slices.Clone(st.Messages)
	}
	r.messages = slices.DeleteFunc(r.messages, func(m convMessage) bool { return slices.Contains(st.Drop, m.Exchange+":"+m.Role) })
	for i, m := range r.messages {
		if text, ok := st.Edit[m.Exchange+":"+m.Role]; ok {
			r.messages[i].Text = text
		}
	}
	r.messages = append(r.messages, st.Add...)
	r.segments = st.Segments
	return r
}

// event is a message as the conversation source stores it
// (docs/connector-contract.md, "Conversation (Kern)"): one event per message,
// a revision whose token follows its words.
func (m convMessage) event(src, thread string) connector.Event {
	author := connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: conversationOwner}
	if m.Role == conversation.RoleAssistant {
		author = connector.Identity{Source: src, Kind: connector.IdentityAgent, NativeID: conversationAgent}
	}
	native, _ := json.Marshal(conversation.Native{Conversation: thread, Exchange: m.Exchange, Role: m.Role, Interrupted: m.Interrupted})
	artifact := conversation.Artifact(thread, m.Exchange, m.Role)
	sum := sha256.Sum256([]byte(m.Text))
	token := hex.EncodeToString(sum[:8])
	return connector.Event{
		Source: src, NativeID: artifact + "@" + token, Kind: conversation.Kind, Time: m.At,
		Payload: connector.Payload{
			Artifact: artifact, BaseKind: connector.KindMessage, Thread: thread, Text: m.Text, Author: &author,
			Container: connector.Container{Kind: conversation.ContainerStream, NativeID: conversationOwner},
			Revision:  &connector.Revision{Token: token}, Native: native,
		},
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: conversationOwner}},
	}
}

func (r resolved) events(src string) []connector.Event {
	out := make([]connector.Event, len(r.messages))
	for i, m := range r.messages {
		out[i] = m.event(src, r.thread)
	}
	return out
}

func (r resolved) lines(src string) []l1.ConversationLine { return l1.ConversationLines(r.events(src)) }

// eventID is the id of a message in this state.
func (r resolved) eventID(src, exchange, role string) string {
	for _, m := range r.messages {
		if m.Exchange == exchange && m.Role == role {
			ev := m.event(src, r.thread)
			return connector.EventID(src, ev.NativeID)
		}
	}
	panic("no such message " + exchange + ":" + role)
}

func topicLabel(topic string) string {
	return strings.ToLower(strings.Join(strings.Fields(topic), " "))
}

// documents are the segments the state's recorded boundaries make, by label.
func (r resolved) documents(t *testing.T, src string) map[string]l1.Document {
	t.Helper()
	docs, err := l1.BuildConversationSegments(src, r.thread, r.lines(src), r.segments, nil, testRepo(src))
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, span := range r.segments {
		if label := topicLabel(span.Topic); !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	out := map[string]l1.Document{}
	for i, doc := range docs {
		out[labels[i]] = doc
	}
	return out
}

// recording is the model and Jev answers of the states a test distils.
type recording struct {
	fixtures *llm.Fixtures
	jev      *outcomeJev
	seen     map[string]bool
}

func newRecording(t *testing.T) *recording {
	return &recording{fixtures: llm.NewFixtures(), jev: newOutcomeJev(t), seen: map[string]bool{}}
}

func (rec *recording) add(t *testing.T, req llm.Request, response any) {
	t.Helper()
	key := llm.FixtureKey(llm.TierDistill, req)
	if rec.seen[key] {
		return
	}
	rec.seen[key] = true
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.fixtures.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: req, Response: llm.Response{JSON: data, StopReason: llm.StopEnd, Model: "recorded"}}); err != nil {
		t.Fatal(err)
	}
}

// state records the answers of one state of a conversation.
func (rec *recording) state(t *testing.T, src string, r resolved) {
	t.Helper()
	rec.add(t, distiller.ConversationSegmentationRequest(r.lines(src), distillBudget()), map[string]any{"segments": r.segments})
	for label, doc := range r.documents(t, src) {
		answer, ok := r.answers[label]
		if !ok {
			t.Fatalf("state %s records no answer for the topic %q", r.name, label)
		}
		rec.add(t, distiller.RequestFor(doc, distillBudget()), answer)
		rec.jev.classify(doc, r.judge[label])
	}
}

func (rec *recording) registry(t *testing.T, src string) llm.Registry {
	t.Helper()
	registry, err := llm.NewFake(testRepo(src).LLM, rec.fixtures)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func (rec *recording) distiller(t *testing.T, pool *pgxpool.Pool, src string) *distiller.Distiller {
	t.Helper()
	d, err := distiller.New(pool, rec.registry(t, src), rec.jev.judgments(), testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// hookRegistry runs a function before each completion, which is how a test
// makes something happen while an attempt is in the middle of a model call.
type hookRegistry struct {
	llm.Registry
	before func(llm.Request) error
}

func (h hookRegistry) Completer(tier llm.Tier) (llm.Completer, error) {
	c, err := h.Registry.Completer(tier)
	if err != nil {
		return nil, err
	}
	return hookCompleter{c, h.before}, nil
}

type hookCompleter struct {
	llm.Completer
	before func(llm.Request) error
}

func (c hookCompleter) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := c.before(req); err != nil {
		return llm.Response{}, err
	}
	return c.Completer.Complete(ctx, req)
}

func isSegmentation(req llm.Request) bool {
	return req.Schema != nil && req.Schema.Name == "conversation_topics"
}

func conversationTarget(src string, r resolved) string { return l1.DocID(src, r.thread) }

func publishedRecord(t *testing.T, pool *pgxpool.Pool, src, thread string) (l1.ConversationDistillation, bool) {
	t.Helper()
	rec, ok, err := l1.New(pool).ConversationDistillationOf(t.Context(), src, thread)
	if err != nil {
		t.Fatal(err)
	}
	return rec, ok
}

func ids(r resolved, src string, messages ...string) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		exchange, role, _ := strings.Cut(m, ":")
		out[i] = r.eventID(src, exchange, role)
	}
	slices.Sort(out)
	return out
}

func allInputs(r resolved, src string) []string {
	var out []string
	for _, ev := range r.events(src) {
		out = append(out, connector.EventID(src, ev.NativeID))
	}
	slices.Sort(out)
	return out
}

func storedDocuments(t *testing.T, pool *pgxpool.Pool, src string) map[string]l1.Stored {
	t.Helper()
	listed, err := l1.New(pool).List(t.Context(), l1.ListOptions{Source: src, Limit: l1.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]l1.Stored{}
	for _, doc := range listed {
		out[doc.ID] = doc
	}
	return out
}

// A conversation with two interleaved topics is one document per topic. The
// topic that resumes is one document; each document's L0 references are exactly
// the messages whose lines it quotes; a message no segment quotes is cited by
// none; an interrupted exchange's owner line is marked; and the documents carry
// the owner's access list alone with no chat document beside them.
func TestInterleavedTopicsAreOneConversationDocumentEach(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	scenario := loadScenario(t, "queue")
	base := scenario.state(t, "base")
	rec := newRecording(t)
	rec.state(t, src, base)
	ingest(t, pool, base.events(src))
	d := rec.distiller(t, pool, src)

	result, err := d.Distill(t.Context(), conversationTarget(src, base))
	if err != nil || !result.Written || !result.Recorded || result.Superseded {
		t.Fatalf("Distill() = %+v, %v", result, err)
	}
	wantDocs := base.documents(t, src)
	docs := storedDocuments(t, pool, src)
	if len(docs) != 2 {
		t.Fatalf("documents = %d, want one per topic: %+v", len(docs), docs)
	}
	owner := connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: conversationOwner}}
	for label, want := range wantDocs {
		got, ok := docs[want.ID]
		if !ok {
			t.Fatalf("no document for the topic %q", label)
		}
		if got.Kind != l1.KindConversationSegment || got.ArtifactClass != config.ArtifactDM || !got.ACL.Equal(owner) {
			t.Errorf("%s: kind %s class %s ACL %+v, want a conversation segment, dm, the owner alone", label, got.Kind, got.ArtifactClass, got.ACL)
		}
		if !slices.Equal(got.L0Refs, want.L0Refs) {
			t.Errorf("%s cites %v, want exactly the quoted messages %v", label, got.L0Refs, want.L0Refs)
		}
	}
	queue, retry := docs[wantDocs["queue database"].ID], docs[wantDocs["retry cap"].ID]
	wantRefs := func(messages ...string) []string {
		out := make([]string, len(messages))
		for i, m := range messages {
			exchange, role, _ := strings.Cut(m, ":")
			out[i] = base.eventID(src, exchange, role)
		}
		return out
	}
	if want := wantRefs("x-1:owner", "x-1:assistant", "x-3:owner", "x-3:assistant"); !slices.Equal(queue.L0Refs, want) {
		t.Errorf("the topic that resumes cites %v, want %v", queue.L0Refs, want)
	}
	if want := wantRefs("x-2:owner", "x-2:assistant", "x-4:owner"); !slices.Equal(retry.L0Refs, want) {
		t.Errorf("retry cap cites %v, want %v", retry.L0Refs, want)
	}
	greetings := ids(base, src, "x-0:owner", "x-0:assistant")
	for _, doc := range docs {
		for _, greeting := range greetings {
			if slices.Contains(doc.L0Refs, greeting) {
				t.Errorf("%s cites a message no segment quotes: %s", doc.ID, greeting)
			}
		}
	}
	if !strings.Contains(retry.RawText, "Owner (interrupted): For the retry cap I think we should") || strings.Contains(queue.RawText, "interrupted") {
		t.Errorf("the interrupted line is marked in the document that quotes it, and in no other:\n%s\n\n%s", retry.RawText, queue.RawText)
	}
	if queue.Body.OutcomeKind != l1.OutcomeDecided || queue.Body.Outcome != "The job queue uses Postgres." || retry.Body.OutcomeKind != l1.OutcomeOpen {
		t.Errorf("outcomes = %s %q / %s", queue.Body.OutcomeKind, queue.Body.Outcome, retry.Body.OutcomeKind)
	}

	record, ok := publishedRecord(t, pool, src, base.thread)
	wantInputs := allInputs(base, src)
	wantDocIDs := []string{wantDocs["queue database"].ID, wantDocs["retry cap"].ID}
	slices.Sort(wantDocIDs)
	if !ok || !slices.Equal(record.Inputs, wantInputs) || !slices.Equal(record.Documents, wantDocIDs) {
		t.Errorf("record = %+v, want every message read and both documents", record)
	}
	for _, id := range greetings {
		if !record.Read(id) {
			t.Errorf("the record does not say the greeting %s was read", id)
		}
	}

	// None of these messages also made a chat document, and the one
	// assertion job is the decided topic's, under the owner's stream.
	var kinds []string
	for _, doc := range docs {
		kinds = append(kinds, string(doc.Kind))
	}
	if slices.ContainsFunc(kinds, func(k string) bool { return k != string(l1.KindConversationSegment) }) {
		t.Errorf("document kinds = %v, want only conversation segments", kinds)
	}
	if keys := assertJobs(t, pool, queue.ID); len(keys) != 1 {
		t.Errorf("assert jobs for the decided topic = %v", keys)
	}
	if keys := assertJobs(t, pool, retry.ID); len(keys) != 0 {
		t.Errorf("assert jobs for the open topic = %v", keys)
	}
}

// Re-distilling an unchanged conversation rewrites no row. A changed rendering
// of one answer updates only the documents quoting it, and the record names the
// new revision. A topic that disappears loses its document, in the same
// transaction as the record that says so.
func TestRedistillingAConversationWritesOnlyWhatChanged(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	scenario := loadScenario(t, "queue")
	base, reworded, gone := scenario.state(t, "base"), scenario.state(t, "reworded"), scenario.state(t, "retry-gone")
	rec := newRecording(t)
	for _, st := range []resolved{base, reworded} {
		rec.state(t, src, st)
	}
	// The model answers differently about the same lines later, so that answer
	// is a recording of its own.
	later := newRecording(t)
	later.state(t, src, gone)
	ingest(t, pool, base.events(src))
	d := rec.distiller(t, pool, src)
	target := conversationTarget(src, base)
	if _, err := d.Distill(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (map[string]time.Time, time.Time) {
		var published time.Time
		if err := pool.QueryRow(t.Context(), `SELECT published_at FROM l1_conversation_distillations WHERE source = $1 AND thread = $2`, src, base.thread).Scan(&published); err != nil {
			t.Fatal(err)
		}
		at := map[string]time.Time{}
		for id, doc := range storedDocuments(t, pool, src) {
			at[id] = doc.DistilledAt
		}
		return at, published
	}
	wantDocs := base.documents(t, src)
	queueID, retryID := wantDocs["queue database"].ID, wantDocs["retry cap"].ID
	before, publishedBefore := snapshot()

	calls := rec.jev.Calls()
	again, err := d.Distill(t.Context(), target)
	if err != nil || again.Written || again.Deleted || again.Recorded || again.Asserting || rec.jev.Calls() != calls {
		t.Fatalf("an unchanged conversation: %+v, %v (Jev asked %d more times)", again, err, rec.jev.Calls()-calls)
	}
	after, publishedAfter := snapshot()
	if !maps.Equal(before, after) || !publishedBefore.Equal(publishedAfter) {
		t.Errorf("a row was rewritten: %v -> %v; record %v -> %v", before, after, publishedBefore, publishedAfter)
	}

	// The answer to the retry question is reworded: a new revision of one
	// artifact, quoted by the retry topic alone.
	ingest(t, pool, reworded.events(src))
	result, err := d.Distill(t.Context(), target)
	if err != nil || !result.Written || !result.Recorded || result.Deleted {
		t.Fatalf("a reworded answer: %+v, %v", result, err)
	}
	moved, _ := snapshot()
	if !moved[queueID].Equal(before[queueID]) {
		t.Errorf("the queue topic does not quote the reworded answer and was rewritten")
	}
	if !moved[retryID].After(before[retryID]) {
		t.Errorf("the retry topic quotes the reworded answer and was not updated")
	}
	retry := storedDocuments(t, pool, src)[retryID]
	if !strings.Contains(retry.RawText, "exponential backoff") || !slices.Contains(retry.L0Refs, reworded.eventID(src, "x-2", "assistant")) || slices.Contains(retry.L0Refs, base.eventID(src, "x-2", "assistant")) {
		t.Errorf("the retry topic = %+v, want the reworded revision quoted and cited", retry)
	}
	record, _ := publishedRecord(t, pool, src, base.thread)
	if !record.Read(reworded.eventID(src, "x-2", "assistant")) || record.Read(base.eventID(src, "x-2", "assistant")) || !slices.Equal(record.Inputs, allInputs(reworded, src)) {
		t.Errorf("record = %+v, want the reworded revision and not the one it replaced", record)
	}

	// The model no longer finds the retry topic.
	result, err = d.Distill(t.Context(), target)
	if err != nil || result.Written {
		t.Fatalf("a repeat: %+v, %v", result, err)
	}
	result, err = later.distiller(t, pool, src).Distill(t.Context(), target)
	if err != nil || !result.Deleted {
		t.Fatalf("a topic that disappears: %+v, %v", result, err)
	}
	remaining := storedDocuments(t, pool, src)
	if _, ok := remaining[retryID]; ok || len(remaining) != 1 {
		t.Errorf("documents = %v, want the retry topic gone", slices.Collect(maps.Keys(remaining)))
	}
	record, _ = publishedRecord(t, pool, src, base.thread)
	if !slices.Equal(record.Documents, []string{queueID}) {
		t.Errorf("record documents = %v, want only the topic that remains", record.Documents)
	}
}

// A segmentation that selects nothing leaves a record that every message was
// read and no document.
func TestAConversationThatSelectsNothingStillLeavesARecord(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	scenario := loadScenario(t, "queue")
	nothing := scenario.state(t, "nothing")
	rec := newRecording(t)
	rec.state(t, src, nothing)
	ingest(t, pool, nothing.events(src))
	result, err := rec.distiller(t, pool, src).Distill(t.Context(), conversationTarget(src, nothing))
	if err != nil || result.Written || !result.Recorded || result.Skipped {
		t.Fatalf("Distill() = %+v, %v", result, err)
	}
	if docs := storedDocuments(t, pool, src); len(docs) != 0 {
		t.Errorf("documents = %+v, want none", docs)
	}
	record, ok := publishedRecord(t, pool, src, nothing.thread)
	if !ok || !slices.Equal(record.Inputs, allInputs(nothing, src)) || len(record.Documents) != 0 {
		t.Errorf("record = %+v, want every message and no document", record)
	}
}

// A failed attempt leaves no record, and neither does one whose process was
// stopped in the middle of a model call. After a restart the conversation is
// distilled and its record published.
func TestAFailedOrStoppedConversationAttemptRecordsNothing(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	scenario := loadScenario(t, "queue")
	base := scenario.state(t, "base")
	rec := newRecording(t)
	rec.state(t, src, base)
	ingest(t, pool, base.events(src))
	target := conversationTarget(src, base)
	none := func() {
		t.Helper()
		if record, ok := publishedRecord(t, pool, src, base.thread); ok {
			t.Fatalf("a record was published: %+v", record)
		}
		if docs := storedDocuments(t, pool, src); len(docs) != 0 {
			t.Fatalf("documents were written: %+v", docs)
		}
	}

	// Nothing was recorded for the model to answer.
	empty, err := llm.NewFake(testRepo(src).LLM, llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	failing, err := distiller.New(pool, empty, rec.jev.judgments(), testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Distill(t.Context(), target); err == nil {
		t.Fatal("an attempt with no model answer succeeded")
	}
	none()

	// The segmentation answers, then the first topic's distillation fails.
	distils := 0
	partial, err := distiller.New(pool, hookRegistry{rec.registry(t, src), func(req llm.Request) error {
		if !isSegmentation(req) {
			if distils++; distils == 2 {
				return errors.New("the provider is down")
			}
		}
		return nil
	}}, rec.jev.judgments(), testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partial.Distill(t.Context(), target); err == nil {
		t.Fatal("an attempt whose second topic failed succeeded")
	}
	none()

	// The process is stopped while the model is answering.
	stopped, cancel := context.WithCancel(t.Context())
	killed, err := distiller.New(pool, hookRegistry{rec.registry(t, src), func(llm.Request) error {
		cancel()
		return context.Canceled
	}}, rec.jev.judgments(), testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := killed.Distill(stopped, target); err == nil {
		t.Fatal("a stopped attempt succeeded")
	}
	none()

	// Jev fails after the model answered.
	mute := newOutcomeJev(t)
	mute.failWith(errors.New("jev is down"))
	judged, err := distiller.New(pool, rec.registry(t, src), mute.judgments(), testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := judged.Distill(t.Context(), target); err == nil {
		t.Fatal("an attempt whose judgments failed succeeded")
	}
	none()

	// A restart: a new process, with everything answering.
	result, err := rec.distiller(t, pool, src).Distill(t.Context(), target)
	if err != nil || !result.Recorded || !result.Written {
		t.Fatalf("after the restart: %+v, %v", result, err)
	}
	record, ok := publishedRecord(t, pool, src, base.thread)
	if !ok || !slices.Equal(record.Inputs, allInputs(base, src)) || len(record.Documents) != 2 {
		t.Errorf("record = %+v, want the whole conversation distilled", record)
	}
}

// A message rewritten, or a new one arriving, while an attempt runs is not in
// what the attempt read. Its record is never published naming revisions it did
// not read, nothing it built from the old read is published, and the newer
// message is distilled and recorded afterwards.
func TestAConversationThatMovesWhileItIsDistilledIsNotPublishedFromTheOldRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		// moved is the message revision the second attempt must record.
		moved func(r resolved, src string) string
	}{
		{"a message is rewritten", "reworded", func(r resolved, src string) string { return r.eventID(src, "x-2", "assistant") }},
		{"a message arrives", "arrived", func(r resolved, src string) string { return r.eventID(src, "x-5", "owner") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newPool(t)
			src := newSource(t)
			scenario := loadScenario(t, "queue")
			base, next := scenario.state(t, "base"), scenario.state(t, tc.state)
			rec := newRecording(t)
			rec.state(t, src, base)
			rec.state(t, src, next)
			ingest(t, pool, base.events(src))
			target := conversationTarget(src, base)

			moved := false
			racing, err := distiller.New(pool, hookRegistry{rec.registry(t, src), func(req llm.Request) error {
				if isSegmentation(req) && !moved {
					moved = true
					ingest(t, pool, next.events(src))
				}
				return nil
			}}, rec.jev.judgments(), testConfig(src))
			if err != nil {
				t.Fatal(err)
			}
			result, err := racing.Distill(t.Context(), target)
			if err != nil || !result.Superseded || result.Written || result.Recorded {
				t.Fatalf("the attempt that was moved under: %+v, %v", result, err)
			}
			if record, ok := publishedRecord(t, pool, src, base.thread); ok {
				t.Fatalf("a record was published by an attempt whose inputs moved: %+v", record)
			}
			if docs := storedDocuments(t, pool, src); len(docs) != 0 {
				t.Fatalf("documents were published from the old read: %+v", docs)
			}
			var pending int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE kind = $1 AND target_id = $2 AND state = 'pending'`, distiller.JobKindName, target).Scan(&pending); err != nil || pending != 1 {
				t.Fatalf("pending distill jobs for the moved conversation = %d, %v; want the newer message to be distilled afterwards", pending, err)
			}

			result, err = rec.distiller(t, pool, src).Distill(t.Context(), target)
			if err != nil || !result.Recorded {
				t.Fatalf("the later attempt: %+v, %v", result, err)
			}
			record, ok := publishedRecord(t, pool, src, base.thread)
			if !ok || !record.Read(tc.moved(next, src)) || !slices.Equal(record.Inputs, allInputs(next, src)) {
				t.Errorf("record = %+v, want the newer revision read", record)
			}
		})
	}
}

// The pump enqueues one distill job per conversation however many messages
// arrive, which is what the debounce collapses.
func TestTheChangeFeedEnqueuesOneJobPerConversation(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	scenario := loadScenario(t, "queue")
	base := scenario.state(t, "base")
	ingest(t, pool, base.events(src))
	pump := distiller.NewPump(pool, distiller.PumpOptions{Quiet: time.Minute, MaxDelay: 5 * time.Minute, Batch: l0.MaxLimit})
	progress, err := pump.Once(t.Context())
	if err != nil || progress.Events != len(base.messages) {
		t.Fatalf("Once() = %+v, %v", progress, err)
	}
	rows, err := pool.Query(t.Context(), `SELECT target_id FROM queue_job WHERE kind = $1 AND state = 'pending'`, distiller.JobKindName)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	if want := []string{conversationTarget(src, base)}; !slices.Equal(targets, want) {
		t.Errorf("jobs = %v, want %v", targets, want)
	}
}

// Only the owner's own words become the owner's stance. A decision only Kern's
// answer makes produces none, whatever the model cited and whatever Jev
// answered; the same decision in the owner's words produces a stance whose
// evidence is the conversation document.
func TestOnlyTheOwnersWordsBecomeTheOwnersStance(t *testing.T) {
	cases := []struct {
		state  string
		stance bool
	}{
		{"kern-only", false},
		{"kern-only-uncited", false},
		{"owner-says", true},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			pool := newScratchPool(t)
			src := newSource(t)
			scenario := loadScenario(t, "cache")
			st := scenario.state(t, tc.state)
			rec := newRecording(t)
			rec.state(t, src, st)
			ingest(t, pool, st.events(src))
			result, err := rec.distiller(t, pool, src).Distill(t.Context(), conversationTarget(src, st))
			if err != nil || !result.Written || result.Asserting != tc.stance {
				t.Fatalf("Distill() = %+v, %v; want asserting = %v", result, err, tc.stance)
			}
			doc := st.documents(t, src)["cache choice"]
			stored, err := l1.New(pool).Get(t.Context(), doc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stance != (stored.Body.OutcomeKind == l1.OutcomeDecided && stored.Body.Outcome != "") {
				t.Errorf("outcome = %s %q, want a decision = %v", stored.Body.OutcomeKind, stored.Body.Outcome, tc.stance)
			}
			if !tc.stance && (stored.Body.Outcome != "" || stored.Body.OutcomeKind != l1.OutcomeNone) {
				t.Errorf("a decision only Kern made is the document's outcome: %s %q", stored.Body.OutcomeKind, stored.Body.Outcome)
			}
			jobs := assertJobs(t, pool, doc.ID)
			if tc.stance != (len(jobs) == 1) {
				t.Errorf("assert jobs = %v, want one = %v", jobs, tc.stance)
			}

			// The assertion worker reads the outcome sentence and nothing the
			// agent said around it.
			request := assertworker.RequestFor(stored.Document, 0)
			if text := request.Messages[0].Text; strings.Contains(text, "Kern") || strings.Contains(text, "Memcached") {
				t.Errorf("the assertion worker is shown what the agent said: %q", text)
			}
			if !tc.stance {
				if stances, err := l2.New(pool).StancesFrom(t.Context(), doc.ID); err != nil || len(stances) != 0 {
					t.Fatalf("stances = %+v, %v; want none from Kern's decision", stances, err)
				}
				return
			}
			stancesFromOwner(t, pool, src, stored, "The cache is Redis.")
		})
	}
}

// stancesFromOwner runs the assertion worker over a conversation document with
// a recorded answer and checks that it wrote one stance whose evidence is the
// document.
func stancesFromOwner(t *testing.T, pool *pgxpool.Pool, src string, stored l1.Stored, position string) {
	t.Helper()
	fixtures := llm.NewFixtures()
	budget := 0
	if tier, ok := testRepo(src).LLM.Tier(llm.TierAssert); ok {
		budget = tier.MaxTokens
	}
	answer, _ := json.Marshal(map[string]any{"assertions": []map[string]string{{"topic_name": "which cache the service uses", "position": position}}})
	if err := fixtures.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(stored.Document, budget),
		Response: llm.Response{JSON: answer, StopReason: llm.StopEnd, Model: "recorded"}}); err != nil {
		t.Fatal(err)
	}
	registry, err := llm.NewFake(testRepo(src).LLM, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	a, err := assertworker.New(pool, registry, testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	a = a.WithJudgments(newOutcomeJev(t).judgments())
	scope := l2.ScopeKey(testRepo(src), src, conversationOwner, nil)
	asserted, err := a.Assert(t.Context(), stored.ID, scope)
	if err != nil || asserted.StancesWritten != 1 {
		t.Fatalf("Assert() = %+v, %v", asserted, err)
	}
	stances, err := l2.New(pool).StancesFrom(t.Context(), stored.ID)
	if err != nil || len(stances) != 1 || stances[0].Position != position || !slices.Equal(stances[0].Evidence, []string{stored.ID}) {
		t.Fatalf("stances = %+v, %v; want one, resting on %s", stances, err, stored.ID)
	}
	if !stances[0].ACL.Equal(stored.ACL) {
		t.Errorf("the stance's access list = %+v, want the document's %+v", stances[0].ACL, stored.ACL)
	}
}

// The rebuild an operator deletion queues keeps working for conversation
// documents: the job names a topic, the whole conversation is rebuilt from what
// is still readable, and the record follows.
func TestADeletionRebuildsTheConversationDocumentsThatQuotedIt(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ctx := t.Context()
	scenario := loadScenario(t, "queue")
	base, after := scenario.state(t, "base"), scenario.state(t, "after-deleting-the-answer")
	rec := newRecording(t)
	rec.state(t, src, base)
	rec.state(t, src, after)
	ingest(t, pool, base.events(src))
	d := rec.distiller(t, pool, src)
	if _, err := d.Distill(ctx, conversationTarget(src, base)); err != nil {
		t.Fatal(err)
	}
	wantDocs := base.documents(t, src)
	queueID, retryID := wantDocs["queue database"].ID, wantDocs["retry cap"].ID
	kernsAnswer := base.eventID(src, "x-1", "assistant")
	retryBefore := storedDocuments(t, pool, src)[retryID]

	applied, err := deletion.Apply(ctx, pool, testRepo(src), deletion.Selector{Event: kernsAnswer}, "forget that", "kyle")
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if !slices.Equal(applied.Documents, []string{queueID}) {
		t.Fatalf("the deletion queued %v, want only the topic that quoted the answer", applied.Documents)
	}
	// The queued job is for the topic's document; the distiller rebuilds the
	// conversation from it.
	if err := d.Handle(ctx, queue.Job{TargetID: queueID}); err != nil {
		t.Fatalf("Handle(%s) = %v", queueID, err)
	}
	docs := storedDocuments(t, pool, src)
	rebuilt := docs[after.documents(t, src)["queue database"].ID]
	if strings.Contains(rebuilt.RawText, "one fewer service") || slices.Contains(rebuilt.L0Refs, kernsAnswer) || strings.Contains(rebuilt.Text, "suggested Postgres") {
		t.Errorf("the rebuilt topic still holds the deleted answer: %+v", rebuilt)
	}
	if got := docs[retryID]; !got.DistilledAt.Equal(retryBefore.DistilledAt) {
		t.Errorf("a topic that never quoted the answer was rewritten")
	}
	record, ok := publishedRecord(t, pool, src, base.thread)
	if !ok || record.Read(kernsAnswer) || !slices.Equal(record.Inputs, allInputs(after, src)) {
		t.Errorf("record = %+v, want the conversation without the deleted answer", record)
	}
	record2, err := deletion.Show(ctx, pool, applied.Deletion)
	if err != nil || len(record2.Documents) != 1 || record2.Documents[0].Outcome != l0.RebuiltRedistilled {
		t.Errorf("deletion = %+v, %v; want the topic recorded as re-distilled", record2, err)
	}
}

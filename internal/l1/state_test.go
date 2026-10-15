package l1_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestBuildCopiesCurrentRootState(t *testing.T) {
	label := " In review "
	for _, tt := range []struct {
		name  string
		state *connector.State
	}{
		{"absent", nil},
		{"core only", &connector.State{Core: connector.CoreStateStarted}},
		{"native verbatim", &connector.State{Core: connector.CoreStateBlocked, Native: &label}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, children := pullRequest()
			root.Payload.State = &connector.State{Core: connector.CoreStateProposed}
			// Input takes current revisions only: supply a newer revision replacing
			// the earlier state, including removal of that state.
			current := revised(root, "current", at(8))
			current.Payload.State = tt.state
			doc, err := l1.Build(l1.Input{Root: current, Children: children, Repo: testRepo})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(doc.State, tt.state) {
				t.Fatalf("state = %+v, want %+v", doc.State, tt.state)
			}
			if doc.L0Refs[0] != connector.EventID(current.Source, current.NativeID) {
				t.Fatal("did not use current root revision")
			}
			for _, fixture := range []string{`{"summary":"Work proposed","outcome_kind":"proposed"}`, `{"summary":"Work resolved","outcome_kind":"resolved"}`, `{"summary":"Unrelated body","outcome_kind":"none"}`} {
				var body l1.Body
				if err := json.Unmarshal([]byte(fixture), &body); err != nil {
					t.Fatal(err)
				}
				result, _, err := doc.WithBody(body)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result.State, tt.state) {
					t.Fatalf("body %s changed state", fixture)
				}
			}
			if tt.state != nil {
				if doc.State == tt.state {
					t.Fatal("state aliases source")
				}
				if tt.state.Native != nil && doc.State.Native == tt.state.Native {
					t.Fatal("native aliases source")
				}
			}
		})
	}
}

func TestDerivedDocumentsCopyRootState(t *testing.T) {
	for _, tt := range []struct {
		name  string
		kind  connector.Kind
		state *connector.State
	}{
		{"wiki with state", connector.KindDocument, &connector.State{Core: connector.CoreStateCompleted}},
		{"wiki without state", connector.KindDocument, nil},
		{"valid transcript", connector.KindTranscript, nil},
		{"valid transcript extension", "test.transcript", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := event(tt.kind, "file-1", at(0), who("u1", "kpenfound"), "Notes", "# A\nFirst.\n# B\nSecond.")
			if !tt.kind.IsCore() {
				root.Payload.BaseKind = connector.KindTranscript
			}
			root.Payload.State = tt.state
			if err := root.Validate(); err != nil {
				t.Fatal(err)
			}
			var docs []l1.Document
			var err error
			if tt.kind == connector.KindDocument {
				docs, err = l1.BuildWikiSections(root, nil, testRepo)
			} else {
				docs, err = l1.BuildMeetingSegments(root, []l1.MeetingSpan{{Topic: "A", Start: 1, End: 2}, {Topic: "B", Start: 3, End: 4}}, nil, testRepo)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(docs) != 2 {
				t.Fatalf("documents = %d", len(docs))
			}
			for _, doc := range docs {
				if !reflect.DeepEqual(doc.State, tt.state) {
					t.Errorf("state = %+v, want %+v", doc.State, tt.state)
				}
			}
		})
	}
}

func TestDocumentValidatesState(t *testing.T) {
	empty, long, invalid := "", strings.Repeat("x", 65), string([]byte{0xff})
	for _, tt := range []struct {
		name  string
		state *l1.State
		valid bool
	}{
		{"absent", nil, true}, {"valid", &l1.State{Core: connector.CoreStateStarted}, true},
		{"missing core", &l1.State{}, false}, {"unknown core", &l1.State{Core: "closed"}, false},
		{"empty native", &l1.State{Core: connector.CoreStateStarted, Native: &empty}, false},
		{"long native", &l1.State{Core: connector.CoreStateStarted, Native: &long}, false},
		{"invalid UTF-8", &l1.State{Core: connector.CoreStateStarted, Native: &invalid}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := pullRequest()
			doc, err := l1.Build(l1.Input{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			doc, _, err = doc.WithBody(l1.Body{Summary: "A summary", OutcomeKind: l1.OutcomeNone})
			if err != nil {
				t.Fatal(err)
			}
			doc.State = tt.state
			err = doc.Validate()
			if tt.valid && err != nil || !tt.valid && !errors.Is(err, l1.ErrInvalidDocument) {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

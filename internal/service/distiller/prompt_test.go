package distiller_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestRequestForIsValidForEveryKind(t *testing.T) {
	doc := l1.Document{RawText: "what was said"}
	for _, kind := range l1.Kinds() {
		t.Run(string(kind), func(t *testing.T) {
			doc.Kind = kind
			req := distiller.RequestFor(doc, 2048)
			if err := req.Validate(); err != nil {
				t.Fatalf("RequestFor(%s) is not a request this abstraction would send: %v", kind, err)
			}
			if req.Schema == nil {
				t.Fatal("RequestFor() asked for no schema: a distillation is structured output")
			}
			if err := req.Schema.Validate(); err != nil {
				t.Fatalf("the %s schema is not one internal/llm can enforce: %v", kind, err)
			}
		})
	}
}

var kindFields = []struct {
	kind   l1.Kind
	has    []string
	hasNot string
}{
	{kind: l1.KindIssue, has: []string{"question"}, hasNot: "change"},
	{kind: l1.KindPR, has: []string{"change"}, hasNot: "question"},
	{kind: l1.KindCommit, has: []string{"change"}, hasNot: "question"},
	{kind: l1.KindChatThread, has: []string{"question"}, hasNot: "change"},
}

func schemaOf(kind l1.Kind) *llm.Schema {
	return distiller.RequestFor(l1.Document{Kind: kind, RawText: "x"}, 0).Schema
}

func TestSchemaDeclaresOnlyTheFieldsAKindHas(t *testing.T) {
	for _, tt := range kindFields {
		t.Run(string(tt.kind), func(t *testing.T) {
			schema := schemaOf(tt.kind)
			for _, field := range append([]string{"summary", "outcome", "open_questions"}, tt.has...) {
				if !strings.Contains(string(schema.Definition), `"`+field+`"`) {
					t.Errorf("the %s schema does not ask for %q", tt.kind, field)
				}
			}
			if strings.Contains(string(schema.Definition), `"`+tt.hasNot+`"`) {
				t.Errorf("the %s schema asks for %q, which that kind does not have", tt.kind, tt.hasNot)
			}
		})
	}
}

func TestSchemaRefusesAFieldTheKindDoesNotDeclare(t *testing.T) {
	for _, tt := range kindFields {
		t.Run(string(tt.kind), func(t *testing.T) {
			answer := `{"summary":"a summary","` + tt.hasNot + `":"x"}`
			if err := schemaOf(tt.kind).ValidateJSON([]byte(answer)); err == nil {
				t.Errorf("the %s schema accepted a %q it does not declare", tt.kind, tt.hasNot)
			}
		})
	}
}

func TestSchemaRequiresASummaryAndNothingMore(t *testing.T) {
	for _, tt := range kindFields {
		t.Run(string(tt.kind), func(t *testing.T) {
			schema := schemaOf(tt.kind)
			if err := schema.ValidateJSON([]byte(`{"outcome":"x"}`)); err == nil {
				t.Errorf("the %s schema accepted an answer with no summary", tt.kind)
			}
			if err := schema.ValidateJSON([]byte(`{"summary":"a summary"}`)); err != nil {
				t.Errorf("the %s schema refused a summary alone: %v", tt.kind, err)
			}
		})
	}
}

func TestRequestForTruncatesLongDocuments(t *testing.T) {
	long := strings.Repeat("日本語のテキスト ", distiller.MaxPromptBytes/8)
	first := distiller.RequestFor(l1.Document{Kind: l1.KindPR, RawText: long}, 0).Messages[0].Text
	if len(first) > distiller.MaxPromptBytes+200 {
		t.Errorf("the prompt is %d bytes, want it cut to about %d", len(first), distiller.MaxPromptBytes)
	}
	if !strings.Contains(first, "left out") {
		t.Error("the truncated prompt does not say that it is truncated")
	}
	if !utf8.ValidString(first) {
		t.Error("the prompt was cut inside a rune")
	}
}

func TestRequestForLeavesAShortDocumentAlone(t *testing.T) {
	short := l1.Document{Kind: l1.KindPR, RawText: "what was said"}
	if got := distiller.RequestFor(short, 0).Messages[0].Text; got != short.RawText {
		t.Errorf("a short document was rewritten: %q", got)
	}
}

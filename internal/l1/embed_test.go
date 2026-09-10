package l1_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/l1"
)

// What is embedded is the document's text, whole, unless it is longer than the
// bound — and then it is cut on a rune boundary, in the same place every time,
// because a document that embedded to two different vectors on two runs would
// rank differently for no reason anything could see.
func TestEmbedTextIsBoundedAndDeterministic(t *testing.T) {
	// A two-byte rune straddling the cut: the text is one byte short of the
	// bound, then a rune that would be split by it.
	straddling := strings.Repeat("a", l1.MaxEmbedBytes-1) + "é" + strings.Repeat("b", 10)

	for _, tc := range []struct {
		name string
		text string
		want int // the length of what is embedded
	}{
		{name: "empty", text: "", want: 0},
		{name: "short", text: "a distillation", want: len("a distillation")},
		{name: "exactly the bound", text: strings.Repeat("a", l1.MaxEmbedBytes), want: l1.MaxEmbedBytes},
		{name: "one byte over", text: strings.Repeat("a", l1.MaxEmbedBytes+1), want: l1.MaxEmbedBytes},
		{name: "a rune straddling the cut", text: straddling, want: l1.MaxEmbedBytes - 1},
		{
			name: "every rune multi-byte",
			text: strings.Repeat("é", l1.MaxEmbedBytes),
			want: l1.MaxEmbedBytes,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := l1.EmbedText(tc.text)
			if len(got) != tc.want {
				t.Errorf("EmbedText() is %d bytes, want %d", len(got), tc.want)
			}
			if !strings.HasPrefix(tc.text, got) {
				t.Error("EmbedText() is not a prefix of the text it was given")
			}
			if !utf8.ValidString(got) {
				t.Error("EmbedText() cut a rune in half")
			}
			if again := l1.EmbedText(tc.text); again != got {
				t.Error("EmbedText() cut the same text in two different places")
			}
		})
	}
}

package l3

import (
	"strings"
	"unicode/utf8"
)

// MaxLine is the maximum bytes in a descriptive line.
const MaxLine = 200

// Line is the first nonempty line, collapsed and cut at a character boundary.
func Line(text string) string {
	for l := range strings.Lines(text) {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			continue
		}
		if len(l) <= MaxLine {
			return l
		}
		cut := MaxLine - len("…")
		for cut > 0 && !utf8.RuneStart(l[cut]) {
			cut--
		}
		return l[:cut] + "…"
	}
	return ""
}

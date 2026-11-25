package l1

import (
	"regexp"
	"slices"
	"strings"
)

type redaction struct {
	name string
	re   *regexp.Regexp
	with string
}

func marker(name string) string { return "[redacted:" + name + "]" }

var redactions = []redaction{{
	name: "private-key",
	re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z ]{0,40}PRIVATE KEY-----.*?-----END [A-Z ]{0,40}PRIVATE KEY-----`),
	with: marker("private-key"),
}, {
	name: "aws-key",
	re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	with: marker("aws-key"),
}, {
	name: "github-token",
	re:   regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,255}|github_pat_[A-Za-z0-9_]{20,255})\b`),
	with: marker("github-token"),
}, {
	name: "slack-token",
	re:   regexp.MustCompile(`\bxox[abeoprs]-[A-Za-z0-9-]{10,255}\b`),
	with: marker("slack-token"),
}, {
	name: "api-key",
	re:   regexp.MustCompile(`\bsk-(?:[A-Za-z]{2,10}-)?[A-Za-z0-9_-]{16,255}\b`),
	with: marker("api-key"),
}, {
	name: "jwt",
	re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	with: marker("jwt"),
}, {
	name: "url-credentials",
	re:   regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]{1,20}://)[^\s/@:]{1,64}:[^\s/@]{1,256}@`),
	with: "${1}" + marker("url-credentials") + "@",
}, {
	name: "secret",
	re: regexp.MustCompile(`(?i)\b(pass(?:word|wd)?|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key)\b` +
		`(\s*[:=]\s*)("[^"\n]{4,}"|'[^'\n]{4,}'|` + "`" + `[^` + "`" + `\n]{4,}` + "`" + `|[^\s"'` + "`" + `]{4,})`),
	with: "${1}${2}" + marker("secret"),
}, {
	name: "email",
	re:   regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*\.[A-Za-z]{2,24}\b`),
	with: marker("email"),
}}

// Scrub redacts the secrets and personal data it recognises, and reports the
// shapes it took out — never the values, which are what it exists to keep out
// of everything downstream of L0. The shapes are sorted and
// deduplicated, so they are a thing a caller can log and count.
//
// Scrubbing scrubbed text returns it unchanged.
func Scrub(s string) (string, []string) {
	var found []string
	for _, r := range redactions {
		if !r.re.MatchString(s) {
			continue
		}
		replaced := r.re.ReplaceAllString(s, r.with)
		if replaced == s {
			continue
		}
		s = replaced
		found = append(found, r.name)
	}
	slices.Sort(found)
	return s, slices.Compact(found)
}

func scrubBody(b Body) (Body, []string) {
	var found []string
	scrub := func(s string) string {
		out, kinds := Scrub(s)
		found = append(found, kinds...)
		return out
	}
	b.Summary = scrub(b.Summary)
	b.Question = scrub(b.Question)
	b.Outcome = scrub(b.Outcome)
	b.Change = scrub(b.Change)
	for i, name := range b.CodeNames {
		b.CodeNames[i] = scrub(name)
	}
	for i, q := range b.OpenQuestions {
		b.OpenQuestions[i] = scrub(q)
	}
	slices.Sort(found)
	return b, slices.Compact(found)
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

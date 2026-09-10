package l1

import (
	"regexp"
	"slices"
	"strings"
)

// The scrub runs over every string a document stores before it is written:
// Text, RawText, and the body a model produced. L0 keeps what the source
// actually said — it is append-only and it is the record — but L1 is what
// search returns and what a bundle quotes, so a credential pasted into a pull
// request comment stops here.
//
// Two things about it are worth knowing before adding a pattern:
//
//   - It is idempotent. Scrubbing scrubbed text changes nothing, because every
//     replacement is a marker that no pattern matches the inside of. That is
//     what lets a document be rebuilt from L0 and compare equal to the row that
//     is already there.
//   - It is a net, not a proof. It catches the credential shapes that are
//     recognisable without knowing what a team's secrets look like. It is the
//     last of the four access-control control points, not the first: the first
//     is not ingesting the container at all (docs/design.md#access-control).

// redaction is what a scrubbed value is replaced by, per pattern. The marker
// names the shape rather than the value, so a person reading a document can see
// that something was taken out and what sort of thing it was.
type redaction struct {
	// name is the shape, and the word the marker carries. It is also what
	// [Scrub] reports, which is the only thing about a redaction that may be
	// logged (ADR-0008).
	name string
	re   *regexp.Regexp
	// with is the replacement template. `${1}` and friends keep the part of the
	// match that is not the secret — the scheme of a URL, the key of an
	// assignment — so that what is left still reads as a sentence.
	with string
}

// marker is what a redaction of this shape is written as.
func marker(name string) string { return "[redacted:" + name + "]" }

// redactions are applied in this order, and the order matters: the shapes that
// name a specific kind of credential run before the assignment pattern, which
// would otherwise swallow them into the vaguer `secret` marker.
//
// Every replacement is a marker, and no pattern here matches a marker's
// contents: `[redacted:aws-key]` holds no `@`, no `://`, no long run of base64
// and no `=`. That is what makes the whole set idempotent, and there is a test
// that says so.
var redactions = []redaction{{
	// A private key block, which is the one shape that spans lines.
	name: "private-key",
	re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z ]{0,40}PRIVATE KEY-----.*?-----END [A-Z ]{0,40}PRIVATE KEY-----`),
	with: marker("private-key"),
}, {
	name: "aws-key",
	re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	with: marker("aws-key"),
}, {
	// GitHub's own tokens: `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_` and the
	// fine-grained `github_pat_`.
	name: "github-token",
	re:   regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,255}|github_pat_[A-Za-z0-9_]{20,255})\b`),
	with: marker("github-token"),
}, {
	name: "slack-token",
	re:   regexp.MustCompile(`\bxox[abeoprs]-[A-Za-z0-9-]{10,255}\b`),
	with: marker("slack-token"),
}, {
	// The `sk-` family, which is what most model providers issue.
	name: "api-key",
	re:   regexp.MustCompile(`\bsk-(?:[A-Za-z]{2,10}-)?[A-Za-z0-9_-]{16,255}\b`),
	with: marker("api-key"),
}, {
	// A JSON web token: three base64url segments, the first of which decodes
	// to an object and therefore starts `eyJ`.
	name: "jwt",
	re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	with: marker("jwt"),
}, {
	// Credentials in a URL. The scheme and the host stay: what the link points
	// at is context, and only the userinfo is the secret.
	name: "url-credentials",
	re:   regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]{1,20}://)[^\s/@:]{1,64}:[^\s/@]{1,256}@`),
	with: "${1}" + marker("url-credentials") + "@",
}, {
	// `password=`, `api_key: ...`, `token = "..."`. The key stays so the
	// sentence still says what was taken out.
	name: "secret",
	re: regexp.MustCompile(`(?i)\b(pass(?:word|wd)?|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key)\b` +
		`(\s*[:=]\s*)("[^"\n]{4,}"|'[^'\n]{4,}'|` + "`" + `[^` + "`" + `\n]{4,}` + "`" + `|[^\s"'` + "`" + `]{4,})`),
	with: "${1}${2}" + marker("secret"),
}, {
	// An email address is personal data rather than a credential, and it is the
	// PII half of what design.md asks for. The identity mapping is where an
	// address belongs (docs/config.md); a document quotes people by principal
	// id, so nothing here needs one.
	name: "email",
	re:   regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*\.[A-Za-z]{2,24}\b`),
	with: marker("email"),
}}

// Scrub redacts the secrets and personal data it recognises, and reports the
// shapes it took out — never the values, which are what it exists to keep out
// of everything downstream of L0 (ADR-0008). The shapes are sorted and
// deduplicated, so they are a thing a caller can log and count.
//
// Scrubbing scrubbed text returns it unchanged.
func Scrub(s string) (string, []string) {
	var found []string
	for _, r := range redactions {
		// Checking first keeps the common case — text with nothing in it —
		// from allocating a new string per pattern.
		if !r.re.MatchString(s) {
			continue
		}
		replaced := r.re.ReplaceAllString(s, r.with)
		if replaced == s {
			// A pattern that matched its own marker and changed nothing is not
			// a redaction: reporting one would make an already-scrubbed
			// document look as though it had a secret in it.
			continue
		}
		s = replaced
		found = append(found, r.name)
	}
	slices.Sort(found)
	return s, slices.Compact(found)
}

// scrubBody scrubs everything a model wrote. A model reads the artifact, so a
// summary can quote a credential the artifact contained even though the text it
// was given had already been scrubbed — a token split across lines, a shape no
// pattern here knows. Scrubbing the answer costs nothing and closes that.
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
	for i, q := range b.OpenQuestions {
		b.OpenQuestions[i] = scrub(q)
	}
	slices.Sort(found)
	return b, slices.Compact(found)
}

// trimLines is what every stored string goes through before it is compared or
// written: trailing space on a line, and space around the whole, are
// differences that are not differences, and two runs that differ only in them
// would rewrite a row that has not changed.
func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

package work

import (
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The two record kinds this source accepts beside project, workstream and
// update. They are the request kinds, and the words artifact ids carry.
const (
	KindDocumentRecord = "document"
	KindDecisionRecord = "decision"
)

// Deciders are the parties a decision names as having decided it.
const (
	DeciderOwner        = "owner"
	DeciderChiefOfStaff = "chief_of_staff"
	DeciderCommittee    = "committee"
)

// Deciders lists the values decider accepts.
func Deciders() []string { return []string{DeciderOwner, DeciderChiefOfStaff, DeciderCommittee} }

const (
	maxRejectedOptions = 32
	maxQuestionLen     = 256
	maxLinkLen         = 2048
)

var (
	documentFields = fieldSet("kind", "project", "workstream", "document", "name", "body", "url", "author", "created_at", "updated_at", "deleted")
	decisionFields = fieldSet("kind", "project", "workstream", "decision", "context", "outcome", "rationale", "rejected_options", "decider",
		"supersedes", "spec_document", "pull_request", "question", "author", "created_at", "updated_at", "deleted")
	// recordOnly are the fields no project, workstream or update may carry.
	recordOnly = []string{"document", "body", "decision", "context", "outcome", "rationale", "rejected_options", "decider",
		"supersedes", "spec_document", "pull_request", "question"}
)

func fieldSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// kindName names a request's kind in a message without echoing untrusted text.
func (r Request) kindName() string {
	switch r.Kind {
	case "project", "workstream", "update", KindDocumentRecord, KindDecisionRecord:
		return "a " + r.Kind
	}
	return "this kind"
}

// checkShape refuses a field on a kind it does not belong to. Presence counts,
// so an empty field in the wrong place is refused rather than dropped.
func (r Request) checkShape() error {
	var allowed map[string]bool
	switch r.Kind {
	case KindDocumentRecord:
		allowed = documentFields
	case KindDecisionRecord:
		allowed = decisionFields
	default:
		for _, f := range recordOnly {
			if r.fields[f] {
				return bad("%s belongs only on a document or decision", f)
			}
		}
		return nil
	}
	for _, f := range slices.Sorted(mapKeys(r.fields)) {
		if !allowed[f] {
			return bad("%s does not belong on a %s", f, r.Kind)
		}
	}
	return nil
}

func mapKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// validateRecord checks the content of a document or decision that is not a
// deletion. Shape and ids were checked already.
func (r Request) validateRecord() error {
	if r.Kind == KindDocumentRecord {
		if blank(r.Name) {
			return bad("name is required on a document")
		}
		if blank(r.Body) {
			return bad("body is required on a document")
		}
		if r.fields["url"] && !webURL(r.URL) {
			return bad("url must be an absolute http or https URL of at most %d bytes", maxLinkLen)
		}
		return nil
	}
	if blank(r.Outcome) {
		return bad("outcome is required on a decision")
	}
	switch r.Decider {
	case DeciderOwner, DeciderChiefOfStaff, DeciderCommittee:
	case "":
		return bad("decider is required on a decision: one of %s", strings.Join(Deciders(), ", "))
	default:
		return bad("decider must be one of %s", strings.Join(Deciders(), ", "))
	}
	if len(r.RejectedOptions) > maxRejectedOptions {
		return bad("rejected_options has at most %d entries", maxRejectedOptions)
	}
	for _, option := range r.RejectedOptions {
		if blank(option) {
			return bad("rejected_options entries must not be empty")
		}
	}
	if r.fields["supersedes"] {
		switch {
		case !validID(r.Supersedes):
			return bad("supersedes must be a decision id: 1–128 bytes of letters, digits, -, _, . and /")
		case r.Supersedes == r.Decision:
			return bad("a decision cannot supersede itself")
		}
	}
	if r.fields["spec_document"] && !validID(r.SpecDocument) {
		return bad("spec_document must be a document id: 1–128 bytes of letters, digits, -, _, . and /")
	}
	if r.fields["pull_request"] && !webURL(r.PullRequest) {
		return bad("pull_request must be an absolute http or https URL of at most %d bytes", maxLinkLen)
	}
	if r.fields["question"] && !validQuestion(r.Question) {
		return bad("question must be 1–%d bytes with no whitespace or control characters", maxQuestionLen)
	}
	return nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func webURL(s string) bool {
	if s == "" || len(s) > maxLinkLen || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func validQuestion(s string) bool {
	return s != "" && len(s) <= maxQuestionLen && utf8.ValidString(s) &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

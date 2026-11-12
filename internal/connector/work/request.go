package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
)

// Request is one artifact or its deletion. Presence is retained so that even
// an empty field on the wrong kind is refused, rather than silently dropped.
type Request struct {
	Kind        string               `json:"kind"`
	Project     string               `json:"project"`
	Workstream  string               `json:"workstream,omitempty"`
	Update      string               `json:"update,omitempty"`
	Name        string               `json:"name,omitempty"`
	Description string               `json:"description,omitempty"`
	Text        string               `json:"text,omitempty"`
	State       *connector.CoreState `json:"state,omitempty"`
	Status      string               `json:"status,omitempty"`
	Repos       []Repo               `json:"repos,omitempty"`
	Touches     []connector.Touch    `json:"touches,omitempty"`
	URL         string               `json:"url,omitempty"`
	Author      string               `json:"author,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at,omitempty"`
	Deleted     bool                 `json:"deleted,omitempty"`

	// A document names itself with Document and has a Name and a Body. A
	// decision names itself with Decision and carries the rest.
	Document        string   `json:"document,omitempty"`
	Body            string   `json:"body,omitempty"`
	Decision        string   `json:"decision,omitempty"`
	Context         string   `json:"context,omitempty"`
	Outcome         string   `json:"outcome,omitempty"`
	Rationale       string   `json:"rationale,omitempty"`
	RejectedOptions []string `json:"rejected_options,omitempty"`
	Decider         string   `json:"decider,omitempty"`
	Supersedes      string   `json:"supersedes,omitempty"`
	SpecDocument    string   `json:"spec_document,omitempty"`
	PullRequest     string   `json:"pull_request,omitempty"`
	Question        string   `json:"question,omitempty"`
	fields          map[string]bool
}

// Repo names a repository's container, which need not yet have been ingested.
type Repo struct {
	Source  string `json:"source"`
	Project string `json:"project"`
}

// Native retains source ids, native status and the authenticated poster. On a
// document or decision it also names the work source the record is about and,
// on a decision, keeps the structured fields apart from the prose in the
// payload's text, so that a later layer reads them without parsing it.
type Native struct {
	Project    string `json:"project"`
	Workstream string `json:"workstream,omitempty"`
	Update     string `json:"update,omitempty"`
	Status     string `json:"status,omitempty"`
	Poster     string `json:"poster"`

	Document        string    `json:"document,omitempty"`
	Decision        string    `json:"decision,omitempty"`
	WorkSource      string    `json:"work_source,omitempty"`
	Context         string    `json:"context,omitempty"`
	Outcome         string    `json:"outcome,omitempty"`
	Rationale       string    `json:"rationale,omitempty"`
	RejectedOptions []string  `json:"rejected_options,omitempty"`
	Decider         string    `json:"decider,omitempty"`
	Supersedes      string    `json:"supersedes,omitempty"`
	SupersedesAt    *Location `json:"supersedes_at,omitempty"`
	SpecDocument    string    `json:"spec_document,omitempty"`
	SpecDocumentAt  *Location `json:"spec_document_at,omitempty"`
	PullRequest     string    `json:"pull_request,omitempty"`
	Question        string    `json:"question,omitempty"`
}

// Location is where a record was held when another record named it. It is
// evidence of what a reference resolved to at posting time. Reads resolve the
// id again, because a deleted record's id can be held elsewhere later.
type Location struct {
	Source   string `json:"source"`
	Artifact string `json:"artifact"`
}

// UnmarshalJSON preserves field presence while retaining strict decoding,
// including unknown fields inside repos and touches.
func (r *Request) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	// Inspect the shape before decoding values: state and status are forbidden
	// on updates regardless of their JSON types. Leave type errors to the
	// strict decoder below, and keep deletions subject to their own shape rule.
	var kind string
	var deleted bool
	for name, field := range fields {
		switch {
		case strings.EqualFold(name, "kind"):
			_ = json.Unmarshal(field, &kind)
		case strings.EqualFold(name, "deleted"):
			_ = json.Unmarshal(field, &deleted)
		}
	}
	if kind == "update" && !deleted {
		for name := range fields {
			if strings.EqualFold(name, "state") || strings.EqualFold(name, "status") {
				return bad("state and status are not allowed on an update")
			}
		}
	}
	type plain Request
	var value plain
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return err
	}
	value.fields = map[string]bool{}
	for name, field := range fields {
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return fmt.Errorf("%s must not be null", name)
		}
		value.fields[strings.ToLower(name)] = true
	}
	*r = Request(value)
	return nil
}

func bad(format string, args ...any) error {
	return push.Refuse(push.ErrBadRequest, fmt.Sprintf(format, args...))
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case b == '-', b == '_', b == '.', b == '/':
		default:
			return false
		}
	}
	return true
}

func (r Request) artifact() (string, error) {
	if !validID(r.Project) {
		return "", bad("project must be 1–128 bytes of letters, digits, -, _, . and /")
	}
	artifact := r.Project
	switch r.Kind {
	case "project":
		if r.fields["workstream"] || r.fields["update"] {
			return "", bad("a project has no workstream or update id")
		}
	case "workstream":
		if !validID(r.Workstream) || r.fields["update"] {
			return "", bad("a workstream needs a valid workstream id and no update id")
		}
		artifact += "#" + r.Workstream
	case "update":
		if !validID(r.Update) {
			return "", bad("an update needs a valid update id")
		}
		if r.fields["workstream"] {
			if !validID(r.Workstream) {
				return "", bad("workstream must be a valid id when given")
			}
			artifact += "#" + r.Workstream
		}
		artifact += ":update:" + r.Update
	case KindDocumentRecord, KindDecisionRecord:
		id, other, noun := r.Document, r.Decision, "decision"
		if r.Kind == KindDecisionRecord {
			id, other, noun = r.Decision, r.Document, "document"
		}
		if !validID(id) {
			return "", bad("a %s needs a valid %s id", r.Kind, r.Kind)
		}
		if r.fields["update"] || r.fields[noun] || other != "" {
			return "", bad("a %s carries its own id only, with no update, document or decision id of another kind", r.Kind)
		}
		if r.fields["workstream"] {
			if !validID(r.Workstream) {
				return "", bad("workstream must be a valid id when given")
			}
			artifact += "#" + r.Workstream
		}
		artifact += ":" + r.Kind + ":" + id
	default:
		return "", bad("kind must be project, workstream, update, document or decision")
	}
	return artifact, nil
}

func (c *Connector) validate(r Request) (string, error) {
	if c.ownerOf != "" && r.Kind != KindDocumentRecord && r.Kind != KindDecisionRecord {
		return "", bad("an owner source holds only documents and decisions; post %s to its work source %s", r.kindName(), c.ownerOf)
	}
	artifact, err := r.artifact()
	if err != nil {
		return "", err
	}
	if err := r.checkShape(); err != nil {
		return "", err
	}
	if r.Deleted {
		for field := range r.fields {
			switch field {
			case "kind", "project", "workstream", "update", "document", "decision", "deleted":
			default:
				return "", bad("a deletion carries only ids and deleted: true")
			}
		}
		return artifact, nil
	}
	if r.Kind == "update" && (r.fields["state"] || r.fields["status"]) {
		return "", bad("state and status are not allowed on an update")
	}
	if r.CreatedAt.IsZero() {
		return "", bad("created_at is required RFC 3339")
	}
	if r.fields["updated_at"] && (r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt)) {
		return "", bad("updated_at must be RFC 3339 and not earlier than created_at")
	}
	if r.Kind == KindDocumentRecord || r.Kind == KindDecisionRecord {
		return artifact, r.validateRecord()
	}
	if r.Kind == "update" {
		if r.Text == "" {
			return "", bad("text is required on an update")
		}
		if r.fields["name"] || r.fields["description"] {
			return "", bad("name and description are not allowed on an update")
		}
	} else {
		if r.Name == "" {
			return "", bad("name is required on a project or workstream")
		}
		if r.fields["text"] {
			return "", bad("text belongs only on an update; use description")
		}
	}
	if r.State != nil && !r.State.Valid() {
		return "", bad("state must be a core state")
	}
	if r.fields["status"] && r.State == nil {
		return "", bad("status requires state")
	}
	if len(r.Status) > connector.MaxStateNativeLen {
		return "", bad("status must be at most 64 bytes")
	}
	if r.Kind != "project" && r.fields["repos"] {
		return "", bad("repos is allowed only on a project")
	}
	if r.Kind != "workstream" && r.fields["touches"] {
		return "", bad("touches is allowed only on a workstream")
	}
	if len(r.Repos) > connector.MaxTouches || len(r.Touches) > connector.MaxTouches {
		return "", bad("repos and touches have at most 16 entries")
	}
	for _, repo := range r.Repos {
		if !c.sources[repo.Source] || !connector.ValidSourceID(repo.Source) || repo.Project == "" {
			return "", bad("each repo needs a configured source id and a project")
		}
	}
	paths := 0
	for _, touch := range r.Touches {
		if !c.sources[touch.Source] || !connector.ValidSourceID(touch.Source) || touch.Project == "" {
			return "", bad("each touch needs a configured source id and a project")
		}
		paths += len(touch.Paths)
	}
	if paths > connector.MaxPaths {
		return "", bad("touches has at most 300 paths in total")
	}
	return artifact, nil
}

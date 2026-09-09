package connector_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

var eventTime = time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

// validEvent is the event the validation cases mutate: a GitHub issue comment,
// with everything the contract requires and nothing more.
func validEvent() connector.Event {
	return connector.Event{
		Source:   "github-acme",
		NativeID: "acme/api#12:comment:998",
		Kind:     connector.KindMessage,
		Time:     eventTime,
		Payload: connector.Payload{
			Artifact:  "acme/api#12:comment:998",
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/api", Name: "acme/api"},
			Text:      "we should hand-run the migration",
			Author: &connector.Identity{
				Source:   "github-acme",
				Kind:     connector.IdentityUser,
				NativeID: "MDQ6VXNlcjE=",
				Handle:   "kpenfound",
			},
			Parent: "acme/api#12",
			Thread: "acme/api#12",
		},
		ACL: connector.ACL{{Kind: connector.ACLPublic}},
	}
}

func TestEventIDRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		nativeID string
		wantID   string
	}{
		{
			name:     "github native id is readable",
			source:   "github-acme",
			nativeID: "acme/api#12:comment:998",
			wantID:   "evt:github-acme:acme/api#12:comment:998",
		},
		{
			name:     "a revision hangs off the artifact",
			source:   "discord-eng",
			nativeID: "1234567890@1725812345.0001",
			wantID:   "evt:discord-eng:1234567890@1725812345.0001",
		},
		{
			name:     "unsafe bytes are escaped",
			source:   "drive-team",
			nativeID: "folder id/file 100% done",
			wantID:   "evt:drive-team:folder%20id/file%20100%25%20done",
		},
		{
			name:     "non-ascii is escaped",
			source:   "wiki",
			nativeID: "seite-ü",
			wantID:   "evt:wiki:seite-%C3%BC",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := connector.EventID(tt.source, tt.nativeID)
			if got != tt.wantID {
				t.Errorf("EventID(%q, %q) = %q, want %q", tt.source, tt.nativeID, got, tt.wantID)
			}
			source, nativeID, err := connector.ParseEventID(got)
			if err != nil {
				t.Fatalf("ParseEventID(%q) = %v, want no error", got, err)
			}
			if source != tt.source || nativeID != tt.nativeID {
				t.Errorf("ParseEventID(%q) = (%q, %q), want (%q, %q)", got, source, nativeID, tt.source, tt.nativeID)
			}
		})
	}
}

// Whoever sizes the id column reads MaxEventIDLen, so it has to be the bound
// the encoding actually produces rather than the native id's length: a native
// id of non-ASCII text passes validation and encodes to three bytes per byte.
func TestEventIDLengthBound(t *testing.T) {
	source := strings.Repeat("s", connector.MaxSourceIDLen)
	nativeID := strings.Repeat("ü", connector.MaxNativeIDLen/2) // two bytes each, both escaped
	ev := connector.Event{Source: source, NativeID: nativeID}
	if len(ev.NativeID) != connector.MaxNativeIDLen {
		t.Fatalf("the fixture is %d bytes, want %d", len(ev.NativeID), connector.MaxNativeIDLen)
	}

	// This is the worst case exactly: the longest source, and a native id of
	// nothing but bytes that percent-encode to three each.
	if got, want := len(connector.EventID(source, nativeID)), connector.MaxEventIDLen; got != want {
		t.Errorf("EventID() on the worst case is %d bytes, and MaxEventIDLen says %d", got, want)
	}
}

func TestEventIDIsStableAcrossReEmission(t *testing.T) {
	// Idempotency rests on this: the same observation emitted twice must land
	// on the same id, or ingest cannot deduplicate it.
	first := connector.EventID("github-acme", "acme/api#12")
	second := connector.EventID("github-acme", "acme/api#12")
	if first != second {
		t.Errorf("EventID is not stable: %q then %q", first, second)
	}
	if edited := connector.EventID("github-acme", "acme/api#12@2"); edited == first {
		t.Errorf("a revision has the same id as the original: %q", edited)
	}
}

func TestParseEventIDErrors(t *testing.T) {
	tests := []struct{ name, id string }{
		{"no prefix", "github-acme:acme/api#12"},
		{"no native id", "evt:github-acme"},
		{"empty source", "evt::acme/api#12"},
		{"source is not a source id", "evt:GitHub:acme/api#12"},
		{"truncated escape", "evt:github-acme:a%2"},
		{"bad escape", "evt:github-acme:a%zz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := connector.ParseEventID(tt.id); err == nil {
				t.Errorf("ParseEventID(%q) = nil error, want error", tt.id)
			}
		})
	}
}

func TestEventValidate(t *testing.T) {
	tests := []struct {
		name string
		// mutate turns the valid event into the case under test. A nil mutate
		// is the valid event itself.
		mutate  func(*connector.Event)
		wantErr bool
	}{
		{name: "the valid event"},
		{
			name: "an extension kind with a base kind",
			mutate: func(e *connector.Event) {
				e.Kind = "figma.comment"
				e.Payload.BaseKind = connector.KindMessage
			},
		},
		{
			name: "a tombstone",
			mutate: func(e *connector.Event) {
				e.Kind = connector.KindTombstone
				e.Payload.Author = nil
				e.Payload.Text = ""
				e.Payload.Target = e.Payload.Artifact
				e.NativeID += ":tombstone"
				e.Payload.Artifact = e.NativeID
			},
		},
		{
			name: "a tombstone that is a revision of what it retracts",
			mutate: func(e *connector.Event) {
				e.Kind = connector.KindTombstone
				e.Payload.Author = nil
				e.Payload.Text = ""
				e.Payload.Target = e.Payload.Artifact
			},
			wantErr: true,
		},
		{
			name: "an edit is a revision of the artifact",
			mutate: func(e *connector.Event) {
				e.NativeID = e.Payload.Artifact + "@2"
				e.Payload.Revision = &connector.Revision{Token: "2", EditedAt: eventTime}
			},
		},
		{
			name: "a revision token that disagrees with the native id",
			mutate: func(e *connector.Event) {
				e.NativeID = e.Payload.Artifact + "@2"
				e.Payload.Revision = &connector.Revision{Token: "9", EditedAt: eventTime}
			},
			wantErr: true,
		},
		{
			name: "a revision in the native id and no payload.revision",
			mutate: func(e *connector.Event) {
				e.NativeID = e.Payload.Artifact + "@2"
			},
			wantErr: true,
		},
		{
			name:    "a payload.revision with no revision in the native id",
			mutate:  func(e *connector.Event) { e.Payload.Revision = &connector.Revision{Token: "2"} },
			wantErr: true,
		},
		{
			name: "a native id ending in @ with no token",
			mutate: func(e *connector.Event) {
				e.NativeID = e.Payload.Artifact + "@"
				e.Payload.Revision = &connector.Revision{Token: "2"}
			},
			wantErr: true,
		},
		{
			name: "an artifact that contains an @ of its own",
			mutate: func(e *connector.Event) {
				e.Kind = connector.KindCommit
				e.NativeID = "acme/api@0b5ed1f"
				e.Payload.Artifact = e.NativeID
			},
		},
		{
			name:    "empty acl",
			mutate:  func(e *connector.Event) { e.ACL = nil },
			wantErr: true,
		},
		{
			name:    "acl group with no native id",
			mutate:  func(e *connector.Event) { e.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: "github-acme"}} },
			wantErr: true,
		},
		{
			name:    "acl group with no source",
			mutate:  func(e *connector.Event) { e.ACL = connector.ACL{{Kind: connector.ACLGroup, NativeID: "acme/api"}} },
			wantErr: true,
		},
		{
			name:    "public acl entry that names something",
			mutate:  func(e *connector.Event) { e.ACL = connector.ACL{{Kind: connector.ACLPublic, NativeID: "acme/api"}} },
			wantErr: true,
		},
		{
			name:    "unknown acl kind",
			mutate:  func(e *connector.Event) { e.ACL = connector.ACL{{Kind: "everyone"}} },
			wantErr: true,
		},
		{
			name:    "source is not a source id",
			mutate:  func(e *connector.Event) { e.Source = "GitHub Acme" },
			wantErr: true,
		},
		{
			name:    "empty native id",
			mutate:  func(e *connector.Event) { e.NativeID, e.Payload.Artifact = "", "" },
			wantErr: true,
		},
		{
			name: "native id over the limit",
			mutate: func(e *connector.Event) {
				e.NativeID = strings.Repeat("x", connector.MaxNativeIDLen+1)
				e.Payload.Artifact = e.NativeID
			},
			wantErr: true,
		},
		{
			name: "native id with whitespace",
			mutate: func(e *connector.Event) {
				e.NativeID = "acme/api#12 comment 998"
				e.Payload.Artifact = e.NativeID
			},
			wantErr: true,
		},
		{
			name:    "id that is not the derived id",
			mutate:  func(e *connector.Event) { e.ID = "evt:github-acme:something-else" },
			wantErr: true,
		},
		{
			name:   "the derived id set explicitly",
			mutate: func(e *connector.Event) { e.ID = connector.EventID(e.Source, e.NativeID) },
		},
		{
			name:    "unknown kind",
			mutate:  func(e *connector.Event) { e.Kind = "gossip" },
			wantErr: true,
		},
		{
			name:    "extension kind without a base kind",
			mutate:  func(e *connector.Event) { e.Kind = "figma.comment" },
			wantErr: true,
		},
		{
			name: "extension kind with a base kind that is not core",
			mutate: func(e *connector.Event) {
				e.Kind = "figma.comment"
				e.Payload.BaseKind = "figma.thing"
			},
			wantErr: true,
		},
		{
			name:    "core kind carrying a base kind",
			mutate:  func(e *connector.Event) { e.Payload.BaseKind = connector.KindMessage },
			wantErr: true,
		},
		{
			name:    "zero time",
			mutate:  func(e *connector.Event) { e.Time = time.Time{} },
			wantErr: true,
		},
		{
			name:    "no artifact",
			mutate:  func(e *connector.Event) { e.Payload.Artifact = "" },
			wantErr: true,
		},
		{
			name:    "native id is not the artifact or a revision of it",
			mutate:  func(e *connector.Event) { e.Payload.Artifact = "acme/api#13" },
			wantErr: true,
		},
		{
			name:    "no container",
			mutate:  func(e *connector.Event) { e.Payload.Container = connector.Container{} },
			wantErr: true,
		},
		{
			name:    "container with no native id",
			mutate:  func(e *connector.Event) { e.Payload.Container.NativeID = "" },
			wantErr: true,
		},
		{
			name:    "a kind that requires an author without one",
			mutate:  func(e *connector.Event) { e.Payload.Author = nil },
			wantErr: true,
		},
		{
			name: "an issue with neither title nor text",
			mutate: func(e *connector.Event) {
				e.Kind = connector.KindIssue
				e.Payload.Text = ""
			},
			wantErr: true,
		},
		{
			name: "a message with no text is fine: it may be an attachment",
			mutate: func(e *connector.Event) {
				e.Payload.Text = ""
			},
		},
		{
			name:    "author with a display name instead of a native id",
			mutate:  func(e *connector.Event) { e.Payload.Author.NativeID = "" },
			wantErr: true,
		},
		{
			name:    "author with an unknown identity kind",
			mutate:  func(e *connector.Event) { e.Payload.Author.Kind = "human" },
			wantErr: true,
		},
		{
			name: "participant with an unknown role",
			mutate: func(e *connector.Event) {
				e.Payload.Participants = []connector.Participant{{Identity: *e.Payload.Author, Role: "watcher"}}
			},
			wantErr: true,
		},
		{
			name: "participant with a bad identity",
			mutate: func(e *connector.Event) {
				e.Payload.Participants = []connector.Participant{{Identity: connector.Identity{Source: "github-acme"}, Role: connector.RoleReviewer}}
			},
			wantErr: true,
		},
		{
			name: "mention with a bad identity",
			mutate: func(e *connector.Event) {
				e.Payload.Mentions = []connector.Identity{{Source: "github-acme", Kind: connector.IdentityUser}}
			},
			wantErr: true,
		},
		{
			name:    "tombstone without a target",
			mutate:  func(e *connector.Event) { e.Kind = connector.KindTombstone },
			wantErr: true,
		},
		{
			name:    "target on something that is not a tombstone",
			mutate:  func(e *connector.Event) { e.Payload.Target = "acme/api#12" },
			wantErr: true,
		},
		{
			name: "revision without a token",
			mutate: func(e *connector.Event) {
				e.NativeID = e.Payload.Artifact + "@2"
				e.Payload.Revision = &connector.Revision{EditedAt: eventTime}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := validEvent()
			if tt.mutate != nil {
				tt.mutate(&ev)
			}
			err := ev.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want an error")
				}
				if !errors.Is(err, connector.ErrInvalidEvent) {
					t.Errorf("Validate() = %v, want an error wrapping ErrInvalidEvent", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() = %v, want no error", err)
			}
		})
	}
}

// A rejected event is a bug in a connector someone has to find, so the error
// names the field rather than the event.
func TestValidateNamesTheFieldThatIsWrong(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*connector.Event)
		want   string
	}{
		{
			name:   "an extension kind with no base kind",
			mutate: func(e *connector.Event) { e.Kind = "figma.comment" },
			want:   "base_kind",
		},
		{
			name:   "no artifact",
			mutate: func(e *connector.Event) { e.Payload.Artifact = "" },
			want:   "payload.artifact",
		},
		{
			name:   "an author with no native id",
			mutate: func(e *connector.Event) { e.Payload.Author.NativeID = "" },
			want:   "payload.author.native_id",
		},
		{
			name:   "an acl entry that names nothing",
			mutate: func(e *connector.Event) { e.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: "github-acme"}} },
			want:   "acl[0]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := validEvent()
			tt.mutate(&ev)
			err := ev.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestKindValid(t *testing.T) {
	tests := []struct {
		kind      connector.Kind
		wantValid bool
		wantCore  bool
	}{
		{kind: connector.KindMessage, wantValid: true, wantCore: true},
		{kind: connector.KindTombstone, wantValid: true, wantCore: true},
		{kind: "figma.comment", wantValid: true},
		{kind: "jira.sprint_change", wantValid: true},
		{kind: "figma.comment.reply", wantValid: false},
		{kind: "Figma.comment", wantValid: false},
		{kind: "figma.", wantValid: false},
		{kind: ".comment", wantValid: false},
		{kind: "figma", wantValid: false},
		{kind: "", wantValid: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := tt.kind.Valid(); got != tt.wantValid {
				t.Errorf("Kind(%q).Valid() = %v, want %v", tt.kind, got, tt.wantValid)
			}
			if got := tt.kind.IsCore(); got != tt.wantCore {
				t.Errorf("Kind(%q).IsCore() = %v, want %v", tt.kind, got, tt.wantCore)
			}
		})
	}
}

// The JSON encoding is the contract for a connector that is not written in Go,
// so the field names are pinned here rather than left to the struct tags.
func TestEventJSONIsTheWireFormat(t *testing.T) {
	ev := validEvent()
	// An edit, so that the revision fields are on the wire too.
	ev.NativeID = ev.Payload.Artifact + "@2"
	ev.ID = connector.EventID(ev.Source, ev.NativeID)
	ev.Payload.Revision = &connector.Revision{Token: "2", EditedAt: eventTime}
	ev.Payload.Native = json.RawMessage(`{"reactions":3}`)

	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal() = %v, want no error", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error", err)
	}
	for _, key := range []string{"id", "source", "native_id", "kind", "time", "payload", "acl"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("event JSON has no %q field: %s", key, encoded)
		}
	}
	payload, ok := generic["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is not an object: %s", encoded)
	}
	for _, key := range []string{"artifact", "container", "text", "author", "parent", "thread", "revision", "native"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload JSON has no %q field: %s", key, encoded)
		}
	}
	if got := generic["time"]; got != "2026-09-09T12:00:00Z" {
		t.Errorf("time = %v, want an RFC 3339 timestamp", got)
	}

	var back connector.Event
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal() = %v, want no error", err)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("Validate() after a JSON round trip = %v, want no error", err)
	}
	if back.ID != ev.ID || back.Payload.Artifact != ev.Payload.Artifact || !back.Time.Equal(ev.Time) {
		t.Errorf("round trip = %+v, want %+v", back, ev)
	}
}

// An empty payload field is left out, so an event from a source with little
// metadata does not carry a wall of nulls into JSONB.
func TestEventJSONOmitsEmptyPayloadFields(t *testing.T) {
	ev := validEvent()
	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal() = %v, want no error", err)
	}
	for _, key := range []string{"revision", "target", "native", "base_kind", "title", "mentions", "links"} {
		if strings.Contains(string(encoded), `"`+key+`"`) {
			t.Errorf("event JSON carries an empty %q: %s", key, encoded)
		}
	}
}

func TestCoreKinds(t *testing.T) {
	kinds := connector.CoreKinds()
	if len(kinds) < 10 {
		t.Fatalf("CoreKinds() returned %d kinds, want the whole vocabulary", len(kinds))
	}
	for i, k := range kinds {
		if !k.IsCore() {
			t.Errorf("CoreKinds()[%d] = %q, which is not a core kind", i, k)
		}
		if i > 0 && kinds[i-1] >= k {
			t.Errorf("CoreKinds() is not sorted: %q before %q", kinds[i-1], k)
		}
	}
}

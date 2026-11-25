package l1

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// KindConversationMessage is the L0 kind of every event a conversation source
// stores (docs/connector-contract.md, "Conversation (Kern)"), spelled here so
// that L1 reads a connector's output without importing the connector.
const KindConversationMessage connector.Kind = "conversation.message"

// The speaker labels a conversation line starts with. They are what the
// segmentation and distillation calls read, and what [OwnerLines] recognises
// again, so a line's author is never guessed from its words.
const (
	// OwnerLabel starts a line the owner said.
	OwnerLabel = "Owner"
	// AgentLabel starts a line the owner's agent said. It is context for the
	// owner's words and never evidence of what the owner decided.
	AgentLabel = "Kern"
	// InterruptedMark follows [OwnerLabel] on the words of an exchange that
	// never completed.
	InterruptedMark = " (interrupted)"
)

// IsConversationMessage reports an event a conversation source stored.
func IsConversationMessage(ev connector.Event) bool {
	return ev.Kind == KindConversationMessage
}

// ConversationSegmentPrefix groups every derived topic of one conversation.
// The length prevents a conversation "a:b" from sharing a prefix with "a".
func ConversationSegmentPrefix(thread string) string {
	return fmt.Sprintf("conversation-segment:%d:%s:", len(thread), thread)
}

// ParseConversationSegmentKey is the conversation a segment's artifact key
// belongs to, and false for any other key. A job for a segment is a job for its
// whole conversation: one message can sit in several topics, and a deleted one
// changes where the others begin.
func ParseConversationSegmentKey(key string) (thread string, ok bool) {
	rest, ok := strings.CutPrefix(key, "conversation-segment:")
	if !ok {
		return "", false
	}
	size, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return "", false
	}
	n, err := strconv.Atoi(size)
	if err != nil || n <= 0 || len(rest) < n+1 || rest[n] != ':' {
		return "", false
	}
	return rest[:n], true
}

// ConversationLine is one message of a conversation as a numbered, labelled
// line: the unit a segmentation call ranges over and a topic quotes.
type ConversationLine struct {
	// Event is the message the line quotes.
	Event connector.Event
	// Owner reports the owner's own words. It is true only for a message the
	// owner authored, with the owner role; everything else, including a message
	// whose role or author cannot be read, is the agent's context.
	Owner bool
	// Interrupted marks the owner's words of an exchange that never completed.
	Interrupted bool
}

type conversationNative struct {
	Role        string `json:"role"`
	Interrupted bool   `json:"interrupted"`
}

// ConversationLines orders the current messages of one conversation as the
// document reads them: by when they were said, then by artifact id and native
// id, so two messages said in the same second never swap places between runs.
// Events that are not conversation messages are left out.
func ConversationLines(messages []connector.Event) []ConversationLine {
	ordered := slices.Clone(messages)
	ordered = slices.DeleteFunc(ordered, func(ev connector.Event) bool { return !IsConversationMessage(ev) })
	slices.SortFunc(ordered, byConversationOrder)
	lines := make([]ConversationLine, 0, len(ordered))
	for _, ev := range ordered {
		var native conversationNative
		if len(ev.Payload.Native) != 0 {
			_ = json.Unmarshal(ev.Payload.Native, &native)
		}
		owner := native.Role == "owner" && ev.Payload.Author != nil && ev.Payload.Author.Kind == connector.IdentityUser
		lines = append(lines, ConversationLine{Event: ev, Owner: owner, Interrupted: owner && native.Interrupted})
	}
	return lines
}

// Text is the line as it is quoted: the speaker, then the words on one line.
func (l ConversationLine) Text() string {
	label := AgentLabel
	if l.Owner {
		label = OwnerLabel
		if l.Interrupted {
			label += InterruptedMark
		}
	}
	return label + ": " + strings.Join(strings.Fields(l.Event.Payload.Text), " ")
}

// OwnerLines reports, for each line of a conversation document's raw text,
// whether the owner said it. The label a line starts with decides, and a line's
// words never can: [ConversationLine.Text] puts its own label first.
func OwnerLines(raw string) []bool {
	lines := strings.Split(raw, "\n")
	owner := make([]bool, len(lines))
	for i, line := range lines {
		owner[i] = strings.HasPrefix(line, OwnerLabel+": ") || strings.HasPrefix(line, OwnerLabel+InterruptedMark+": ")
	}
	return owner
}

// BuildConversationSegments builds one document per topic of a conversation
// from validated topic boundaries over its numbered lines. A topic named by
// several ranges is one document, in line order. Each document's L0 references
// are exactly the messages whose lines it quotes, so a message no range takes
// is cited by none.
//
// A line is dropped, with its reference, when its access list shares nothing
// with the lines before it in the topic: a document is readable by whoever may
// read everything it quotes, and one with an empty list is unreadable forever.
// A conversation source gives every message the owner's list alone, so in
// practice the fold is the owner's.
func BuildConversationSegments(source, thread string, lines []ConversationLine, spans []MeetingSpan, resolver *principal.Resolver, repo config.Repo) ([]Document, error) {
	previous := 0
	order := []string{}
	byTopic := map[string][]ConversationLine{}
	for _, span := range spans {
		topic := strings.ToLower(strings.Join(strings.Fields(span.Topic), " "))
		if topic == "" || len(topic) > 160 || span.Start <= previous || span.End < span.Start || span.End > len(lines) {
			return nil, fmt.Errorf("%w: invalid conversation span %d-%d", ErrInvalidDocument, span.Start, span.End)
		}
		previous = span.End
		if _, ok := byTopic[topic]; !ok {
			order = append(order, topic)
		}
		byTopic[topic] = append(byTopic[topic], lines[span.Start-1:span.End]...)
	}
	docs := make([]Document, 0, len(order))
	for _, topic := range order {
		quoted := byTopic[topic]
		for _, line := range quoted {
			if line.Event.Source != source || line.Event.Payload.Thread != thread {
				return nil, fmt.Errorf("%w: message %s is not in conversation %s of source %s", ErrInvalidDocument, line.Event.NativeID, thread, source)
			}
		}
		first := quoted[0].Event
		events := make([]connector.Event, len(quoted))
		for i, line := range quoted {
			events[i] = line.Event
		}
		acl, kept := aclOf(first, events[1:])
		events = append([]connector.Event{first}, kept...)
		quoted = slices.DeleteFunc(slices.Clone(quoted), func(line ConversationLine) bool {
			return line.Event.NativeID != first.NativeID && !slices.ContainsFunc(kept, func(ev connector.Event) bool { return ev.NativeID == line.Event.NativeID })
		})

		raw := make([]string, len(quoted))
		refs := make([]string, len(quoted))
		for i, line := range quoted {
			raw[i] = line.Text()
			refs[i] = connector.EventID(line.Event.Source, line.Event.NativeID)
		}
		scrubbed, _ := Scrub(strings.Join(raw, "\n"))
		if scrubbed = trimLines(scrubbed); scrubbed == "" {
			continue
		}
		sum := sha256.Sum256([]byte(topic))
		key := ConversationSegmentPrefix(thread) + hex.EncodeToString(sum[:12])
		references := References(events, resolver, repo.Code)
		doc := Document{
			ID: DocID(source, key), Kind: KindConversationSegment, ArtifactClass: config.ArtifactDM,
			Source:       Source{System: source, NativeID: key},
			L0Refs:       refs,
			Time:         conversationTimes(events),
			Participants: conversationParticipants(quoted, resolver),
			References:   references, ACL: acl, RawText: scrubbed,
		}
		doc.Scope = scopeOf(first, repo, references)
		docs = append(docs, doc)
	}
	return docs, nil
}

func conversationTimes(events []connector.Event) Times {
	first, last := events[0].Time.UTC(), events[0].Time.UTC()
	for _, ev := range events[1:] {
		at := ev.Time.UTC()
		if at.Before(first) {
			first = at
		}
		last = latest(last, at)
	}
	return Times{Created: first, Updated: last, LastActivity: last}
}

func conversationParticipants(quoted []ConversationLine, resolver *principal.Resolver) []Participant {
	roles := map[string]connector.ParticipantRole{}
	for _, line := range quoted {
		if line.Event.Payload.Author == nil {
			continue
		}
		id := resolvePerson(resolver, *line.Event.Payload.Author)
		if id == "" {
			continue
		}
		role := connector.RoleAgent
		if line.Owner {
			role = connector.RoleAuthor
		}
		if held, ok := roles[id]; !ok || slices.Index(rolePrecedence, role) < slices.Index(rolePrecedence, held) {
			roles[id] = role
		}
	}
	out := make([]Participant, 0, len(roles))
	for id, role := range roles {
		out = append(out, Participant{PrincipalID: id, Role: role})
	}
	slices.SortFunc(out, func(a, b Participant) int {
		if c := cmp.Compare(slices.Index(rolePrecedence, a.Role), slices.Index(rolePrecedence, b.Role)); c != 0 {
			return c
		}
		return cmp.Compare(a.PrincipalID, b.PrincipalID)
	})
	return out
}

// StanceText is the text a stance is read from. A conversation segment's is its
// outcome sentence, which the distiller sets only from the owner's lines; its
// summary and open questions also describe what the agent said, which is
// context and never what the owner decided. Every other document's is its text.
func (d Document) StanceText() string {
	if d.Kind == KindConversationSegment {
		return d.Body.Outcome
	}
	return d.Text
}

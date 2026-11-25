package l1

import (
	"context"
	"slices"

	"github.com/kpenfound/hearsay/internal/l0"
)

// CoverageState is where one message stands with the distillation of its
// conversation.
type CoverageState string

const (
	// CoveragePending is a message held, that no published distillation of its
	// conversation read at its current revision: not consumed from the change
	// feed yet, its attempt failed or was stopped, or it was rewritten or
	// arrived since the last one was published.
	CoveragePending CoverageState = "pending"
	// CoverageCovered is a message the published distillation read at its
	// current revision and quoted in a document the reader may read.
	CoverageCovered CoverageState = "covered"
	// CoverageExcluded is a message that will not be covered: deleted, left out
	// of every document by the distillation of its current revision, or an id
	// that does not name a message this reader may read. The last is answered
	// as a message that does not exist.
	CoverageExcluded CoverageState = "excluded"
)

// Coverage is the answer for one event id.
type Coverage struct {
	// Event is the id as it was asked.
	Event string
	State CoverageState
	// Documents are the ids of the current documents whose L0 refs cite the
	// message's current revision and that the reader may read, sorted. They are
	// only ever present when State is [CoverageCovered].
	Documents []string
}

// Coverage answers, for each event id, one [CoverageState] for its artifact's
// current revision.
//
// The state is decided from the published distillation record
// ([Store.ConversationDistillationsReading]), the current documents' L0 refs and
// what L0 says is current or deleted, and from nothing else: no job state and
// no clock. A message is `covered` or `excluded` only by a distillation that
// read its current revision's event id, so a message rewritten while documents
// of its older revision remain, or one that arrived while an attempt ran, is
// `pending` until a distillation that read it is published.
//
// An id that is not the current or an earlier revision of a conversation
// message, one L0 no longer serves (deleted, retracted), and one the reader may
// not read are all `excluded`, with the same answer as an id that does not
// exist. A message whose documents the reader may not read, by access list or
// reach, is `excluded` too. Reads must run in one snapshot for the three reads
// to agree; the caller's transaction is that.
func (s *Store) Coverage(ctx context.Context, reader Reader, eventIDs []string) ([]Coverage, error) {
	held, err := l0.New(s.db).CurrentOf(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	var current []string
	for _, id := range eventIDs {
		if h, ok := held[id]; ok && messageReadable(reader, h) {
			current = append(current, h.Event)
		}
	}
	read, err := s.ConversationDistillationsReading(ctx, current)
	if err != nil {
		return nil, err
	}
	holders, err := s.Holders(ctx, current)
	if err != nil {
		return nil, err
	}
	out := make([]Coverage, len(eventIDs))
	for i, id := range eventIDs {
		out[i] = Coverage{Event: id, State: CoverageExcluded}
		h, ok := held[id]
		if !ok || !messageReadable(reader, h) {
			continue
		}
		if _, distilled := read[h.Event]; !distilled {
			out[i].State = CoveragePending
			continue
		}
		for _, holder := range holders[h.Event] {
			if reader.Allows(holder.ACL) && reader.InReach(holder.Scope) {
				out[i].Documents = append(out[i].Documents, holder.ID)
			}
		}
		slices.Sort(out[i].Documents)
		if len(out[i].Documents) > 0 {
			out[i].State = CoverageCovered
		}
	}
	return out, nil
}

func messageReadable(reader Reader, h l0.Held) bool {
	return h.Kind == KindConversationMessage && reader.Allows(h.ACL) && reader.Allows(h.Asked)
}

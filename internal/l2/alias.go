package l2

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

// AliasCandidate is a proposed name. It is deliberately absent from Resolve.
// ACL is the intersection of every current evidence document's grants; an
// empty ACL means no reader may see the proposal.
type AliasCandidate struct {
	EntityID string
	Alias    string
	Name     string
	Votes    int
	Evidence []string
	ACL      connector.ACL
}

// NormalizeAlias is the stable key for a proposed name.
func NormalizeAlias(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// VoteAlias records one thread's support for a name connected to a PR. Call
// inside the transaction that writes the L1 document, with its current ACL.
func (s *Store) VoteAlias(ctx context.Context, entityID, name, docID, prDocID string, docACL, prACL connector.ACL) error {
	name = strings.Join(strings.Fields(name), " ")
	alias := NormalizeAlias(name)
	if entityID == "" || alias == "" || docID == "" || prDocID == "" || len(docACL) == 0 || len(prACL) == 0 {
		return fmt.Errorf("%w: incomplete alias vote", ErrInvalid)
	}
	voteACL := intersectAliasACL(docACL, prACL)
	raw, err := json.Marshal(voteACL)
	if err != nil {
		return fmt.Errorf("encoding alias access: %w", err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO l2_alias_candidates(entity_id, alias, name, acl) VALUES ($1,$2,$3,$4)
        ON CONFLICT (entity_id, alias) DO NOTHING`, entityID, alias, name, raw); err != nil {
		return fmt.Errorf("creating alias candidate: %w", err)
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO l2_alias_votes(entity_id,alias,doc_id,pr_doc_id) VALUES ($1,$2,$3,$4)
        ON CONFLICT DO NOTHING`, entityID, alias, docID, prDocID)
	if err != nil {
		return fmt.Errorf("recording alias vote: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	var oldRaw []byte
	if err := s.db.QueryRow(ctx, `SELECT acl FROM l2_alias_candidates WHERE entity_id=$1 AND alias=$2 FOR UPDATE`, entityID, alias).Scan(&oldRaw); err != nil {
		return fmt.Errorf("reading alias access: %w", err)
	}
	var old connector.ACL
	if err := json.Unmarshal(oldRaw, &old); err != nil {
		return fmt.Errorf("decoding alias access: %w", err)
	}
	merged := intersectAliasACL(old, voteACL)
	// The first insert already has this vote's ACL. No grant is broadened.
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encoding alias access: %w", err)
	}
	_, err = s.db.Exec(ctx, `UPDATE l2_alias_candidates SET votes=votes+1,
        evidence=(SELECT array_agg(DISTINCT id ORDER BY id) FROM unnest(evidence || ARRAY[$3,$4]::text[]) id), acl=$5
        WHERE entity_id=$1 AND alias=$2`, entityID, alias, docID, prDocID, mergedRaw)
	if err != nil {
		return fmt.Errorf("updating alias candidate: %w", err)
	}
	return nil
}

// AliasCandidates returns proposals with current evidence restrictions folded
// into their stored ACL. A missing or retracted evidence document closes access.
func (s *Store) AliasCandidates(ctx context.Context, entityID string) ([]AliasCandidate, error) {
	rows, err := s.db.Query(ctx, `SELECT alias,name,votes,evidence,acl FROM l2_alias_candidates WHERE entity_id=$1 ORDER BY alias`, entityID)
	if err != nil {
		return nil, fmt.Errorf("listing alias candidates: %w", err)
	}
	defer rows.Close()
	out := []AliasCandidate{}
	for rows.Next() {
		c := AliasCandidate{EntityID: entityID}
		var raw []byte
		if err := rows.Scan(&c.Alias, &c.Name, &c.Votes, &c.Evidence, &raw); err != nil {
			return nil, fmt.Errorf("listing alias candidates: %w", err)
		}
		if err := json.Unmarshal(raw, &c.ACL); err != nil {
			return nil, fmt.Errorf("decoding alias access: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing alias candidates: %w", err)
	}
	for i := range out {
		aclByID, err := l1.New(s.db).ACLs(ctx, out[i].Evidence)
		if err != nil {
			return nil, err
		}
		for _, id := range out[i].Evidence {
			acl, ok := aclByID[id]
			if !ok {
				out[i].ACL = nil
				break
			}
			out[i].ACL = intersectAliasACL(out[i].ACL, acl)
		}
	}
	return out, nil
}

func intersectAliasACL(a, b connector.ACL) connector.ACL {
	if len(a) == 0 || len(b) == 0 {
		return connector.ACL{}
	}
	public := func(acl connector.ACL) bool {
		return slices.ContainsFunc(acl, func(e connector.ACLEntry) bool { return e.Kind == connector.ACLPublic })
	}
	if public(a) {
		return slices.Clone(b)
	}
	if public(b) {
		return slices.Clone(a)
	}
	out := connector.ACL{}
	for _, e := range a {
		if slices.ContainsFunc(b, func(other connector.ACLEntry) bool {
			return e.Kind == other.Kind && e.Source == other.Source && e.NativeID == other.NativeID
		}) {
			out = append(out, e)
		}
	}
	return out
}

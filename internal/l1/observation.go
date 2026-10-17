package l1

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

// PipelineObservation is one worker stage run. All counts, durations (in
// nanoseconds) and provider costs are nullable: unknown is never a reported
// zero. Include every retry and additional candidate-processing call. Inputs
// supplies the evidence/audience and is validated alongside the judgment batch.
type PipelineObservation struct {
	ID                string
	Stage             string
	Cohort            string
	Inputs            JudgmentInputs
	RecordedAt        time.Time
	HandlerNS         *int64
	QueueDelayNS      *int64
	GenerativeCalls   *int64
	GenerativeUsage   llm.JudgmentUsage
	JudgmentAttempts  *int64
	JudgmentFallbacks *int64
	JudgmentUsage     llm.JudgmentUsage
	CandidateCount    *int64
}

// AddPipelineCount propagates missing, invalid and overflowing counts as
// unknown. It also works for nanosecond durations. A caller accumulating from
// an empty, known cohort starts with an explicit pointer to zero.
func AddPipelineCount(a, b *int64) *int64 {
	if a == nil || b == nil || *a < 0 || *b < 0 || *a > math.MaxInt64-*b {
		return nil
	}
	n := *a + *b
	return &n
}

func knownCount(n *int64) *int64 {
	if n != nil && *n < 0 {
		return nil
	}
	return n
}

func (s *Store) writeObservation(ctx context.Context, o PipelineObservation) error {
	if o.ID == "" || o.Stage == "" {
		return JournalStorageFailure
	}
	llm.SanitizeJudgmentUsage(&o.GenerativeUsage)
	llm.SanitizeJudgmentUsage(&o.JudgmentUsage)
	acl, err := json.Marshal(o.Inputs.Spec.ACL)
	if err != nil {
		return err
	}
	var id string
	return s.db.QueryRow(ctx, `INSERT INTO pipeline_observations
     (id,stage,cohort,documents,events,scope,acl,handler_ns,queue_delay_ns,generative_calls,generative_input_tokens,generative_output_tokens,generative_cost_usd,judgment_attempts,judgment_fallbacks,judgment_input_tokens,judgment_output_tokens,judgment_cost_usd,candidate_count)
     VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING id`,
		o.ID, o.Stage, o.Cohort, orEmpty(o.Inputs.Documents), orEmpty(o.Inputs.Events), orEmpty(o.Inputs.Spec.Scope), acl, knownCount(o.HandlerNS), knownCount(o.QueueDelayNS), knownCount(o.GenerativeCalls), o.GenerativeUsage.InputTokens, o.GenerativeUsage.OutputTokens, o.GenerativeUsage.CostUSD, knownCount(o.JudgmentAttempts), knownCount(o.JudgmentFallbacks), o.JudgmentUsage.InputTokens, o.JudgmentUsage.OutputTokens, o.JudgmentUsage.CostUSD, knownCount(o.CandidateCount)).Scan(&id)
}

// ObservationsFor returns stage accounting filtered by scope, stored ACL and
// the current evidence ACLs. Reporting/aggregation belongs to internal/eval.
func (s *Store) ObservationsFor(ctx context.Context, reader Reader) ([]PipelineObservation, error) {
	q := &query{}
	filter, ok := reader.predicate(q, "")
	if !ok {
		return []PipelineObservation{}, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id,stage,cohort,documents,events,scope,acl,recorded_at,handler_ns,queue_delay_ns,generative_calls,generative_input_tokens,generative_output_tokens,generative_cost_usd,judgment_attempts,judgment_fallbacks,judgment_input_tokens,judgment_output_tokens,judgment_cost_usd,candidate_count FROM pipeline_observations WHERE `+filter+` ORDER BY recorded_at,id`, q.args...)
	if err != nil {
		return nil, JournalStorageFailure
	}
	out := []PipelineObservation{}
	for rows.Next() {
		var o PipelineObservation
		var raw []byte
		if err = rows.Scan(&o.ID, &o.Stage, &o.Cohort, &o.Inputs.Documents, &o.Inputs.Events, &o.Inputs.Spec.Scope, &raw, &o.RecordedAt, &o.HandlerNS, &o.QueueDelayNS, &o.GenerativeCalls, &o.GenerativeUsage.InputTokens, &o.GenerativeUsage.OutputTokens, &o.GenerativeUsage.CostUSD, &o.JudgmentAttempts, &o.JudgmentFallbacks, &o.JudgmentUsage.InputTokens, &o.JudgmentUsage.OutputTokens, &o.JudgmentUsage.CostUSD, &o.CandidateCount); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &o.Inputs.Spec.ACL); err != nil {
			break
		}
		out = append(out, o)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return nil, JournalStorageFailure
	}
	visible := out[:0]
	for _, o := range out {
		allowed, e := s.journalEvidenceReadable(ctx, reader, o.Inputs.Documents, o.Inputs.Events, "")
		if e != nil {
			return nil, e
		}
		if allowed {
			visible = append(visible, o)
		}
	}
	return visible, nil
}

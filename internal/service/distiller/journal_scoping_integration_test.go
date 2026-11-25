//go:build integration

package distiller_test

import (
	"context"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

type outcomeIngestionProvider struct {
	llm.JudgmentProvider
	entered  chan<- struct{}
	ingested <-chan error
}

func (p outcomeIngestionProvider) Judge(ctx context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	close(p.entered)
	select {
	case err := <-p.ingested:
		if err != nil {
			return llm.JudgmentResponse{}, err
		}
	case <-ctx.Done():
		return llm.JudgmentResponse{}, ctx.Err()
	}
	return p.JudgmentProvider.Judge(ctx, req)
}

func TestOutcomeJournalReusedAcrossConcurrentUnrelatedIngestion(t *testing.T) {
	pool := dbtest.Scratch(t, "distiller_none")
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	entered, ingested := make(chan struct{}), make(chan error, 1)

	unrelated := event(src, connector.KindIssue, repo+"#999", at(9), who(src, "u1", "kpenfound"), "Unrelated issue", "A separate discussion.")
	go func() {
		select {
		case <-entered:
		case <-t.Context().Done():
			return
		}
		_, err := l0.New(pool).Append(t.Context(), unrelated)
		ingested <- err
	}()
	j := jevOver(t, outcomeIngestionProvider{fixtureJev(t), entered, ingested}, llm.JudgmentConfig{})
	d := jevDistiller(t, pool, src, j)
	id := l1.DocID(src, repo+"#12")
	result, err := d.Distill(t.Context(), id)
	if err != nil || !result.Written {
		t.Fatalf("distill=%+v %v", result, err)
	}
	records := outcomeRecords(t, pool)
	if len(records) != 4 {
		t.Fatalf("records=%+v", records)
	}
	for _, r := range records {
		if r.State != "committed" {
			t.Fatalf("record=%+v", r)
		}
	}

	restartFake := llm.NewFakeJudgmentProvider()
	restart := jevDistiller(t, pool, src, jevOver(t, restartFake, llm.JudgmentConfig{}))
	result, err = restart.Distill(t.Context(), id)
	if err != nil || result.Written || restartFake.Calls() != 0 {
		t.Fatalf("restart=%+v %v calls=%d", result, err, restartFake.Calls())
	}
	if doc, err := l1.New(pool).Get(t.Context(), id); err != nil || doc.Body.OutcomeKind != l1.OutcomeDecided {
		t.Fatalf("outcome = %+v %v", doc.Body, err)
	}
	observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) < 2 {
		t.Fatalf("missing restart observation: %d", len(observations))
	}
	latest := observations[len(observations)-1]
	if latest.JudgmentRequests == nil || *latest.JudgmentRequests != 0 || latest.JudgmentAttempts == nil || *latest.JudgmentAttempts != 0 || latest.JudgmentFallbacks == nil || *latest.JudgmentFallbacks != 0 {
		t.Fatalf("reused judgments counted again: %+v", latest)
	}
}

//go:build integration

package distiller_test

import (
	"context"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
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
	pool := newScratchPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	entered, ingested := make(chan struct{}), make(chan error, 1)
	// A real L0 append commits on another connection while the judgment is in
	// flight, in the same source but on a different artifact/conversation.
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
	fake := llm.NewFakeJudgmentProvider(outcomeStep(t, "none"))
	j := outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, outcomeIngestionProvider{fake, entered, ingested})
	d := boostedDistiller(t, pool, src, outcomeRegistry(t, false), j, false)
	id := l1.DocID(src, repo+"#12")
	result, err := d.Distill(t.Context(), id)
	if err != nil || !result.Written {
		t.Fatalf("distill=%+v %v", result, err)
	}
	records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskOutcomeClassification))
	if err != nil || len(records) != 1 || records[0].State != "committed" {
		t.Fatalf("records=%+v %v", records, err)
	}
	// Restart with a fresh capability and no judgment response available.
	restartFake := llm.NewFakeJudgmentProvider()
	restart := boostedDistiller(t, pool, src, outcomeRegistry(t, false), outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, restartFake), false)
	result, err = restart.Distill(t.Context(), id)
	if err != nil || result.Written || restartFake.Calls() != 0 {
		t.Fatalf("restart=%+v %v calls=%d", result, err, restartFake.Calls())
	}
	assertBaselineOutcome(t, pool, id)
}

package config

import "github.com/kpenfound/hearsay/internal/llm"

// buildJudgments reads `llm.judgments`, Jev's configuration. Every field is
// optional here: the distiller and the assertion worker refuse to start
// without a model and a key (llm.NewJudgments), and the other commands never
// call Jev.
func (l *loader) buildJudgments() llm.JudgmentConfig {
	cfg := llm.JudgmentConfig{}.WithDefaults()
	defined := ""
	for _, d := range l.llm {
		b := d.v.Judgments
		if b == nil {
			continue
		}
		a := at{file: d.file, line: d.line, noun: "judgments", label: "llm judgments"}
		if defined != "" {
			l.bad(a, "", "judgments are already configured at %s", defined)
			continue
		}
		defined = position(d.file, d.line)
		cfg.Model = b.Model
		cfg.BaseURL = b.BaseURL
		if b.APIKeyEnv != "" {
			cfg.APIKeyEnv = b.APIKeyEnv
		}
		if b.Threshold != nil {
			cfg.Threshold = *b.Threshold
			if cfg.Threshold == 0 {
				l.bad(a, "threshold", "must be above 0 and at most 1")
			}
		}
		cfg.MaxRetries = b.MaxRetries
		cfg.MaxTotalTokens = b.MaxTotalTokens
		cfg.MaxStateQuestionTokens = b.MaxStateQuestionTokens
		cfg.Timeout = l.duration(a, "timeout", b.Timeout, cfg.Timeout)
		cfg.TotalTimeout = l.duration(a, "total_timeout", b.TotalTimeout, cfg.TotalTimeout)
		cfg.Backoff = l.duration(a, "backoff", b.Backoff, cfg.Backoff)
		if err := cfg.Validate(); err != nil {
			l.bad(a, "", "%s", err)
		}
	}
	return cfg.WithDefaults()
}

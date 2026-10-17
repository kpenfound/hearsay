package config

import "github.com/kpenfound/hearsay/internal/llm"

func (l *loader) buildBoost() llm.JudgmentConfig {
	cfg := llm.JudgmentConfig{}.WithDefaults()
	defined := ""
	for _, d := range l.llm {
		b := d.v.Boost
		if b == nil {
			continue
		}
		a := at{file: d.file, line: d.line, noun: "boost", label: "llm boost"}
		if defined != "" {
			l.bad(a, "", "boost is already configured at %s", defined)
			continue
		}
		defined = position(d.file, d.line)
		if b.Enabled != nil {
			enabled, ok := b.Enabled.(bool)
			if !ok {
				l.bad(a, "enabled", "must be true or false")
			} else {
				cfg.Enabled = enabled
			}
		}
		cfg.Model = b.Model
		cfg.BaseURL = b.BaseURL
		if b.APIKeyEnv != "" {
			cfg.APIKeyEnv = b.APIKeyEnv
		}
		cfg.MaxRetries = b.MaxRetries
		cfg.MaxTotalTokens = b.MaxTotalTokens
		cfg.MaxStateQuestionTokens = b.MaxStateQuestionTokens
		cfg.Timeout = l.duration(a, "timeout", b.Timeout, cfg.Timeout)
		cfg.TotalTimeout = l.duration(a, "total_timeout", b.TotalTimeout, cfg.TotalTimeout)
		cfg.Backoff = l.duration(a, "backoff", b.Backoff, cfg.Backoff)
		cfg.Cooldown = l.duration(a, "cooldown", b.Cooldown, cfg.Cooldown)
		cfg.AssertionJobBudget = l.duration(a, "assertion_job_budget", b.AssertionJobBudget, cfg.AssertionJobBudget)
		cfg.CooldownFailures = b.CooldownFailures
		cfg.Policies = map[llm.JudgmentTask]llm.AcceptancePolicy{}
		for task, p := range b.Policies {
			if p.Threshold == nil {
				l.bad(a, "policies", "each task policy requires an explicit threshold")
				continue
			}
			cfg.Policies[llm.JudgmentTask(task)] = llm.AcceptancePolicy{Name: p.Name, ModelVersion: p.ModelVersion, Threshold: *p.Threshold}
		}
		if err := cfg.Validate(); err != nil {
			l.bad(a, "", "%s", err)
		}
	}
	return cfg.WithDefaults()
}

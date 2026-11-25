package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfigValidateAcceptsAWorkAndNotesDeploymentConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"config", "validate", "testdata/apiary-shaped/hearsay.yaml"}, &stdout, &stderr); err != nil {
		t.Fatalf("config validate = %v, want the configuration accepted", err)
	}
	for _, want := range []string{"is valid", "sources", "5", "principals", "model tiers", "distill=claude-sbx/haiku", "assert=claude-sbx/sonnet"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
}

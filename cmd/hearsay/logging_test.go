package main

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config/configtest"
)

const (
	loggedDisplayName = "Priya Raman"
	loggedEmail       = "priya.raman@example.test"
	loggedHandle      = "priya-raman-handle"
	loggedAgentToken  = "agent-token-must-not-be-logged"
	loggedSourceToken = "source-token-must-not-be-logged"
)

func loggingConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("HEARSAY_TEST_AGENT_TOKEN", loggedAgentToken)
	t.Setenv("HEARSAY_TEST_GITHUB_TOKEN", loggedSourceToken)
	return configtest.Write(t, map[string]string{
		"sources/github.yaml":   "id: github\ntype: github\ncontainers: [acme/api]\nsecrets:\n  token: HEARSAY_TEST_GITHUB_TOKEN\n",
		"scopes/api.yaml":       "id: api\nsources: [github]\n",
		"principals/priya.yaml": "id: priya\nname: " + loggedDisplayName + "\nidentities:\n  - {source: github, native_id: u1, handle: " + loggedHandle + "}\n  - {source: github, handle: " + loggedEmail + "}\n",
		"principals/shed.yaml":  "id: shed\nkind: agent\nclass: worker\ntoken_env: HEARSAY_TEST_AGENT_TOKEN\nscopes: ['*']\nidentities: [{source: github, native_id: u2}]\n",
	})
}

func TestInfoLogsCarryNoConfigurationContent(t *testing.T) {
	noDatabase(t)
	_, stderr, err := runCancelled(t, "api", "--config", loggingConfig(t), "--log-level", "info", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if !strings.Contains(stderr, `"service":"api"`) {
		t.Fatalf("stderr = %q, want log lines to check", stderr)
	}
	for _, content := range []string{loggedDisplayName, loggedEmail, loggedHandle} {
		if strings.Contains(stderr, content) {
			t.Errorf("info-level output carries %q:\n%s", content, stderr)
		}
	}
}

func TestDebugLogsCarryNoCredentials(t *testing.T) {
	noDatabase(t)
	_, stderr, err := runCancelled(t, "api", "--config", loggingConfig(t), "--log-level", "debug", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if !strings.Contains(stderr, `"service":"api"`) {
		t.Fatalf("stderr = %q, want log lines to check", stderr)
	}
	for _, credential := range []string{loggedAgentToken, loggedSourceToken} {
		if strings.Contains(stderr, credential) {
			t.Errorf("debug-level output carries a credential:\n%s", stderr)
		}
	}
}

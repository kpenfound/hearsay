package main

import (
	"strings"
	"testing"
)

const (
	loggedPrincipalToken = "principal-token-7c1d9e"
	loggedSourceToken    = "source-token-4b8a02"
)

func loggingConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("HEARSAY_LOGTEST_KYLE_TOKEN", loggedPrincipalToken)
	t.Setenv("HEARSAY_LOGTEST_GITHUB_TOKEN", loggedSourceToken)
	noDatabase(t)
	return writeConfig(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\nsecrets: {token: HEARSAY_LOGTEST_GITHUB_TOKEN}\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"principals/kyle.yaml": "id: kyle\nname: Kyle Penfound\ntoken_env: HEARSAY_LOGTEST_KYLE_TOKEN\n" +
			"identities: [{source: github, handle: kpenfound}, {source: github, handle: kyle@acme.example}]\n",
	})
}

func TestInfoLogsCarryNoConfigurationContent(t *testing.T) {
	_, stderr, err := runCancelled(t, "api", "--config", loggingConfig(t), "--log-level", "info", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if !strings.Contains(stderr, `"msg":"configuration loaded"`) {
		t.Fatalf("the startup log was not captured:\n%s", stderr)
	}
	for _, content := range []string{"Kyle Penfound", "kyle@acme.example", "kpenfound"} {
		if strings.Contains(stderr, content) {
			t.Errorf("info-level output carries %q:\n%s", content, stderr)
		}
	}
}

func TestDebugLogsCarryNoCredentials(t *testing.T) {
	_, stderr, err := runCancelled(t, "api", "--config", loggingConfig(t), "--log-level", "debug", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if !strings.Contains(stderr, `"msg":"configuration loaded"`) {
		t.Fatalf("the startup log was not captured:\n%s", stderr)
	}
	for _, secret := range []string{loggedPrincipalToken, loggedSourceToken} {
		if strings.Contains(stderr, secret) {
			t.Errorf("debug-level output carries a credential %q:\n%s", secret, stderr)
		}
	}
}

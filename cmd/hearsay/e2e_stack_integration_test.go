//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const e2ePollEvery = 100 * time.Millisecond

func waitUntil(t *testing.T, what string, within time.Duration, log interface{ String() string }, check func() string) {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	tick := time.NewTicker(e2ePollEvery)
	defer tick.Stop()
	for {
		why := check()
		if why == "" {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("%s did not happen within %s: %s\n%s", what, within, why, log.String())
		case <-tick.C:
		}
	}
}

func newE2ERun(t *testing.T, tokens map[string]string) *e2eRun {
	t.Helper()
	r := &e2eRun{t: t, hooks: freeAddr(t), apiAddr: freeAddr(t), tokens: tokens, stderr: &syncBuffer{}, evidence: os.Getenv("HEARSAY_E2E_EVIDENCE")}
	if r.evidence != "" {
		if err := os.MkdirAll(r.evidence, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func writeStackConfig(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hearsay.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (r *e2eRun) startAll(dir string, probes ...string) (stop func(), distillerAddr, assertAddr string) {
	t := r.t
	t.Helper()
	distillerAddr, assertAddr = freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"all", "--config", dir, "--listen", r.hooks, "--api-listen", r.apiAddr,
			"--distiller-listen", distillerAddr, "--assert-worker-listen", assertAddr, "--log-format", "json"}, io.Discard, r.stderr)
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("hearsay all stopped with %v\n%s", err, r.stderr.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("hearsay all did not stop")
		}
	}
	t.Cleanup(stop)

	for _, addr := range []string{r.hooks, r.apiAddr, distillerAddr, assertAddr} {
		for _, probe := range probes {
			waitUntil(t, addr+probe+" answering 200", time.Minute, r.stderr, func() string {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+probe, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err.Error()
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return resp.Status
				}
				return ""
			})
		}
	}
	return stop, distillerAddr, assertAddr
}

func e2eLLMSection(stubURL string) string {
	tier := func(name string) string {
		return fmt.Sprintf("    %s:\n      provider: anthropic\n      model: model-stub\n      base_url: %s\n      api_key_env: HEARSAY_E2E_MODEL_KEY\n      max_retries: -1\n", name, stubURL)
	}
	judgments := fmt.Sprintf("  judgments:\n    model: jev-stub\n    base_url: %s\n    api_key_env: HEARSAY_E2E_MODEL_KEY\n    max_retries: -1\n", stubURL)
	return "\nllm:\n  tiers:\n" + tier("distill") + tier("assert") + judgments
}

func createDatabase(t *testing.T, url, prefix string) string {
	t.Helper()
	name := prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgx.Connect(t.Context(), swapDatabaseName(url, "postgres"))
	if err != nil {
		t.Fatalf("connecting to the server: %v", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(t.Context())) }()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		c, err := pgx.Connect(ctx, swapDatabaseName(url, "postgres"))
		if err != nil {
			return
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return swapDatabaseName(url, name)
}

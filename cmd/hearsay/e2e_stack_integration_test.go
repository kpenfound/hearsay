//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/telemetry/logtest"
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
	r := &e2eRun{t: t, hooks: freeAddr(t), apiAddr: freeAddr(t), tokens: tokens, stderr: &logtest.Buffer{}, evidence: os.Getenv("HEARSAY_E2E_EVIDENCE")}
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

func serveModelStub(w http.ResponseWriter, r *http.Request, structured func(tool, text string, properties map[string]json.RawMessage) (any, bool), judge func(state string, ids []string) map[string]float64) {
	if r.URL.Path == "/v1/systemone" {
		var req struct {
			State     string                     `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Questions) == 0 {
			http.Error(w, `{"error":"the stub answers System One calls only"}`, http.StatusBadRequest)
			return
		}
		ids := make([]string, 0, len(req.Questions))
		for id := range req.Questions {
			ids = append(ids, id)
		}
		answers := map[string]any{}
		for id, p := range judge(req.State, ids) {
			answers[id] = map[string]any{"type": "noul", "noul": p}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-stub", "answers": answers, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}})
		return
	}
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if r.URL.Path != "/v1/messages" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Tools) != 1 || len(req.Messages) == 0 {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"the stub answers structured Messages calls only"}}`, http.StatusBadRequest)
		return
	}
	tool := req.Tools[0]
	answer, ok := structured(tool.Name, req.Messages[0].Content, tool.InputSchema.Properties)
	if !ok {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"the stub does not know this schema"}}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_stub", "type": "message", "role": "assistant", "model": "stub", "stop_reason": "tool_use",
		"content": []any{map[string]any{"type": "tool_use", "id": "toolu_stub", "name": tool.Name, "input": answer}},
		"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
	})
}

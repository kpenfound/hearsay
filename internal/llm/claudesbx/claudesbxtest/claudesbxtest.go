// Package claudesbxtest is the fake `sbx` and `claude` programs the claude-sbx
// adapter is tested against, so that no test makes a real model call or needs a
// real Docker Sandboxes install. A test that builds a registry with a
// `claude-sbx` tier, here or in another package, installs them first.
//
// The `sbx` is agentcore's own fake (agenttest.Sbx), which runs the command a
// call gives it on this machine, so the `claude` that runs is the fake in this
// package: it records what it was started with, then prints a recorded
// stream-json answer. Both are put on PATH, which is how the adapter finds them
// (it runs the Runner's defaults, `sbx` and `claude`, and holds no setting for
// either).
package claudesbxtest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent/agenttest"
)

// claude is the fake claude: what it was started with is recorded beside the
// sbx recordings, then the stream the test chose is printed.
const claude = `
printf '%s\n' "$@" > "$CLAUDE_SBX_REC/claude-args.txt"
env > "$CLAUDE_SBX_REC/claude-env.txt"
pwd > "$CLAUDE_SBX_REC/claude-pwd.txt"
ls -A | wc -l > "$CLAUDE_SBX_REC/claude-files.txt"
cat > "$CLAUDE_SBX_REC/claude-stdin.txt"
echo run >> "$CLAUDE_SBX_REC/claude-runs.txt"
cat "$CLAUDE_SBX_REC/stream.jsonl"
`

// Rig is the installed fakes and what they recorded.
type Rig struct {
	// Rec holds what the fake claude recorded, and Sbx the directory the fake
	// sbx lives in, where it records its calls and where its marker files go.
	Rec, Sbx string
}

// Install puts a fake `sbx` and a fake `claude` first on PATH for the test, and
// has the claude print stream. The fakes are under the test's temporary
// directory, which agentcore requires of anything it is told to run as an
// agent. Whatever ANTHROPIC_API_KEY the environment holds is cleared, because
// this provider needs none and a test must show it does not read one.
func Install(t *testing.T, stream string) *Rig {
	t.Helper()
	rec := t.TempDir()
	t.Setenv("CLAUDE_SBX_REC", rec)
	t.Setenv("ANTHROPIC_API_KEY", "")
	sbx := agenttest.Sbx(t, "CLAUDE_SBX_REC")
	fake := agenttest.Script(t, "claude", claude)
	t.Setenv("PATH", filepath.Dir(sbx)+string(os.PathListSeparator)+filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := &Rig{Rec: rec, Sbx: filepath.Dir(sbx)}
	r.SetStream(t, stream)
	return r
}

// SetStream changes what the fake claude prints from now on.
func (r *Rig) SetStream(t *testing.T, stream string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Rec, "stream.jsonl"), []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Mark makes the fake sbx misbehave as agentcore's fake documents: "fail-ls",
// "hang-ls" and "hang-create" among them.
func (r *Rig) Mark(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Sbx, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Lines is a recorded file, one entry per line. A file that was never written
// is empty.
func (r *Rig) Lines(t *testing.T, name string) []string {
	t.Helper()
	for _, dir := range []string{r.Rec, r.Sbx} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		}
	}
	return nil
}

// Runs is how many times the fake claude was started: one per call.
func (r *Rig) Runs(t *testing.T) int { return len(nonEmpty(r.Lines(t, "claude-runs.txt"))) }

// Creates is how many sandboxes `sbx create` was asked for, and Removes how
// many `sbx rm` removed.
func (r *Rig) Creates(t *testing.T) int { return r.calls(t, "create") }

// Removes is how many sandboxes `sbx rm` was asked to remove.
func (r *Rig) Removes(t *testing.T) int { return r.calls(t, "rm") }

func (r *Rig) calls(t *testing.T, verb string) int {
	t.Helper()
	n := 0
	for _, l := range r.Lines(t, "sbx-calls.txt") {
		if strings.HasPrefix(l, verb+" ") {
			n++
		}
	}
	return n
}

// Arg is the value of a flag the fake claude was started with, and whether it
// was there.
func (r *Rig) Arg(t *testing.T, flag string) (string, bool) {
	t.Helper()
	args := r.Lines(t, "claude-args.txt")
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// Env is the environment variable's value as the fake claude saw it, and
// whether it was set.
func (r *Rig) Env(t *testing.T, name string) (string, bool) {
	t.Helper()
	for _, kv := range r.Lines(t, "claude-env.txt") {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Stdin is what the fake claude was given to read: the prompt.
func (r *Rig) Stdin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.Rec, "claude-stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func nonEmpty(in []string) []string {
	return slices.DeleteFunc(slices.Clone(in), func(s string) bool { return s == "" })
}

package tanuki

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// runHook feeds stdinJSON to a hook handler over a real os.Stdin pipe and
// captures what it writes to os.Stdout, exercising the exact I/O path Claude
// Code drives.
func runHook(t *testing.T, fn func(), stdinJSON string) string {
	t.Helper()

	oldIn, oldOut := os.Stdin, os.Stdout

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	os.Stdin, os.Stdout = inR, outW

	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut })

	// Feed stdin and drain stdout concurrently so a large payload cannot
	// deadlock on a full pipe buffer.
	go func() {
		_, _ = inW.WriteString(stdinJSON)
		_ = inW.Close()
	}()

	outCh := make(chan string, 1)

	go func() {
		out, _ := io.ReadAll(outR)
		outCh <- string(out)
	}()

	fn()

	_ = outW.Close()

	return <-outCh
}

// hookEnvelope parses a hook's stdout and returns the hookSpecificOutput
// object, failing if the response is not the envelope Claude Code requires.
func hookEnvelope(t *testing.T, out string) map[string]any {
	t.Helper()

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatalf("hook output is not JSON: %q (%v)", out, err)
	}

	raw, ok := top["hookSpecificOutput"]
	if !ok {
		t.Fatalf("missing hookSpecificOutput envelope: %q", out)
	}

	var hso map[string]any
	if err := json.Unmarshal(raw, &hso); err != nil {
		t.Fatalf("hookSpecificOutput is not an object: %v", err)
	}

	return hso
}

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestHookPreToolUseReversesInput(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	out := runHook(t, hookPreToolUse,
		`{"tool_name":"Bash","tool_input":{"command":"curl http://localhost:9000/x"}}`)

	hso := hookEnvelope(t, out)

	if hso["hookEventName"] != eventPreToolUse {
		t.Errorf("hookEventName = %v, want %q", hso["hookEventName"], eventPreToolUse)
	}

	updated, ok := hso["updatedInput"].(map[string]any)
	if !ok {
		t.Fatalf("updatedInput missing or wrong type in %q", out)
	}

	cmd, _ := updated["command"].(string)
	if cmd != "curl http://amazon.com/x" {
		t.Errorf("command = %q, want the real host restored", cmd)
	}
}

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestHookPostToolUseRewritesOutput(t *testing.T) {
	engDir := setupEngagement(t, testBaseDomain)

	out := runHook(t, hookPostToolUse,
		`{"tool_name":"Bash","tool_output":"reached amazon.com at 52.94.236.248"}`)

	hso := hookEnvelope(t, out)

	if hso["hookEventName"] != eventPostToolUse {
		t.Errorf("hookEventName = %v, want %q", hso["hookEventName"], eventPostToolUse)
	}

	rewritten, _ := hso["updatedToolOutput"].(string)
	if rewritten == "" {
		t.Fatalf("updatedToolOutput missing in %q", out)
	}

	for _, real := range []string{testBaseDomain, testPublicIP} {
		if strings.Contains(rewritten, real) {
			t.Errorf("real value %q leaked in tool output: %s", real, rewritten)
		}
	}

	// The new public IP seen in output must have been auto-mapped.
	if !mappingExists(engDir, "ip", testPublicIP) {
		t.Error("public IP in tool output was not auto-mapped")
	}
}

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestHookUserPromptSubmitAutoMaps(t *testing.T) {
	engDir := setupEngagement(t)

	out := runHook(t, hookUserPromptSubmit,
		`{"prompt":"check bounty.target.com for issues"}`)

	hso := hookEnvelope(t, out)

	if hso["hookEventName"] != eventUserPromptSubmit {
		t.Errorf("hookEventName = %v, want %q", hso["hookEventName"], eventUserPromptSubmit)
	}

	if ctx, _ := hso["additionalContext"].(string); ctx == "" {
		t.Errorf("additionalContext missing in %q", out)
	}

	for _, d := range []string{"target.com", "bounty.target.com"} {
		if !mappingExists(engDir, "domain", d) {
			t.Errorf("domain %q from prompt was not auto-mapped", d)
		}
	}
}

// TestHookEmitsEmptyObjectWhenNothingToDo pins the no-op contract: a tool with
// no rewritable input must emit exactly "{}", not an envelope.
//
//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestHookEmitsEmptyObjectWhenNothingToDo(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	out := runHook(t, hookPreToolUse,
		`{"tool_name":"Bash","tool_input":{"command":"echo nothing to rewrite"}}`)

	if out != "{}" {
		t.Errorf("expected no-op output %q, got %q", "{}", out)
	}
}

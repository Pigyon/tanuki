package tanuki

import (
	"encoding/json"
	"io"
	"os"
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

// TestHookLegacyEventsAreNoops pins the hooks older versions installed: until
// the proxy restarts and rewrites settings.json, Claude Code still calls them,
// and they must answer "{}" rather than fail.
//
//nolint:paralleltest // runHook swaps the process-wide os.Stdin and os.Stdout.
func TestHookLegacyEventsAreNoops(t *testing.T) {
	for _, name := range []string{"post-tool-use", "user-prompt-submit"} {
		out := runHook(t, func() { cmdHook([]string{name}) }, `{"prompt":"check bounty.target.com"}`)
		if out != "{}" {
			t.Errorf("hook %s: got %q, want %q", name, out, "{}")
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

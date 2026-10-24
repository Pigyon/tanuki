package tanuki

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// eventPreToolUse is the hook event name. Claude Code silently ignores the
// payload if hookEventName does not match exactly, so it is a constant.
const eventPreToolUse = "PreToolUse"

var isHookDryRun bool

// emitNoop tells Claude Code the hook has nothing to say. Every path that
// declines to act writes it, so the tool is never blocked.
func emitNoop() {
	fmt.Print("{}")
}

// emitHookOutput writes the hookSpecificOutput envelope. Event-specific keys
// must nest here beside hookEventName; at the top level they are ignored.
func emitHookOutput(eventName string, fields map[string]any) {
	payload := map[string]any{"hookEventName": eventName}

	for k, v := range fields {
		payload[k] = v
	}

	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": payload,
	})
}

func cmdHook(args []string) {
	// A hook must never exit non-zero on a crash: exit 2 tells Claude Code to
	// block the tool. Degrade a panic to a no-op; the proxy still anonymizes.
	defer func() {
		if r := recover(); r != nil {
			if logger != nil {
				logger.Error("hook panic recovered", "panic", r)
			}

			emitNoop()
		}
	}()

	if len(args) == 0 {
		fatal("Usage: tanuki hook [--dry-run] pre-tool-use")
	}

	var filtered []string

	for _, a := range args {
		if a == "--dry-run" {
			isHookDryRun = true
		} else {
			filtered = append(filtered, a)
		}
	}

	if len(filtered) == 0 {
		fatal("Usage: tanuki hook [--dry-run] pre-tool-use")
	}

	switch filtered[0] {
	case "pre-tool-use":
		hookPreToolUse()
	case "post-tool-use", "user-prompt-submit":
		// Installed by older versions; the proxy now does their work. Answer
		// until the proxy's next start rewrites settings.json without them.
		emitNoop()
	default:
		fatal("Unknown hook: " + filtered[0])
	}
}

func readHookInput() (string, map[string]any, bool) {
	// Read only: a hook must not create or switch an engagement. Creating one
	// here switched the active engagement away from the operator's, so the
	// tool then ran with fiction arguments left unrewritten and went to
	// localhost instead of the target. The proxy creates the engagement at
	// startup, before any hook can run.
	eng, err := requireEngagement()
	if err != nil {
		logger.Error("hook: no active engagement", "error", err)
		emitNoop()

		return "", nil, false
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		logger.Error("hook: failed to read stdin", "error", err)
		emitNoop()

		return eng, nil, false
	}

	var data map[string]any

	if err := json.Unmarshal(input, &data); err != nil {
		logger.Error("hook: failed to parse input", "error", err)
		emitNoop()

		return eng, nil, false
	}

	return eng, data, true
}

// toolInputFields lists, per tool, which tool_input fields can carry target
// data and therefore need fiction->real rewriting before the tool runs.
var toolInputFields = map[string][]string{
	"Bash":     {"command"},
	"Edit":     {"file_path", "old_string", "new_string"},
	"Write":    {"file_path", "content"},
	"WebFetch": {"url"},
	"Grep":     {"pattern", "path"},
	"Glob":     {"pattern", "path"},
	"Read":     {"file_path"},
}

func hookPreToolUse() {
	eng, data, ok := readHookInput()
	if !ok {
		return
	}

	engDir := engagementDir(eng)

	mappings := loadMappings(engDir)
	if len(mappings) == 0 {
		emitNoop()
		return
	}

	toolName, _ := data["tool_name"].(string)

	toolInput, _ := data["tool_input"].(map[string]any)
	if toolInput == nil {
		emitNoop()
		return
	}

	f2r := newRewriter(mappings, directionF2R)
	changed := false

	rewrite := func(field string) {
		val, ok := toolInput[field].(string)
		if !ok || val == "" {
			return
		}

		if rewritten := f2r.rewrite(val); rewritten != val {
			toolInput[field] = rewritten
			changed = true
		}
	}

	for _, field := range toolInputFields[toolName] {
		rewrite(field)
	}

	if !changed {
		emitNoop()
		return
	}

	if isHookDryRun {
		logger.Info("dry-run: would rewrite tool input", "tool", toolName)
	}

	// permissionDecision is omitted so rewriting the args does not also approve
	// the call and remove the operator's permission prompt.
	emitHookOutput(eventPreToolUse, map[string]any{"updatedInput": toolInput})
}

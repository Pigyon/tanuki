package tanuki

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Hook event names. Claude Code silently ignores the payload if hookEventName
// does not match exactly, so these are constants, not inline strings.
const (
	eventPreToolUse       = "PreToolUse"
	eventPostToolUse      = "PostToolUse"
	eventUserPromptSubmit = "UserPromptSubmit"
)

var isHookDryRun bool

// emitHookOutput writes the hookSpecificOutput envelope. Event-specific keys
// must nest here beside hookEventName; at the top level they are ignored.
func emitHookOutput(eventName string, fields map[string]interface{}) {
	payload := map[string]interface{}{"hookEventName": eventName}

	for k, v := range fields {
		payload[k] = v
	}

	_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
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

			fmt.Print("{}")
		}
	}()

	if len(args) == 0 {
		fatal("Usage: tanuki hook [--dry-run] <pre-tool-use|post-tool-use|user-prompt-submit>")
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
		fatal("Usage: tanuki hook [--dry-run] <pre-tool-use|post-tool-use|user-prompt-submit>")
	}

	switch filtered[0] {
	case "pre-tool-use":
		hookPreToolUse()
	case "post-tool-use":
		hookPostToolUse()
	case "user-prompt-submit":
		hookUserPromptSubmit()
	default:
		fatal("Unknown hook: " + filtered[0])
	}
}

func readHookInput() (string, map[string]interface{}, bool) {
	eng, err := ensureEngagement()
	if err != nil {
		logger.Error("hook: failed to ensure engagement", "error", err)
		fmt.Print("{}")

		return "", nil, false
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		logger.Error("hook: failed to read stdin", "error", err)
		fmt.Print("{}")

		return eng, nil, false
	}

	var data map[string]interface{}

	if err := json.Unmarshal(input, &data); err != nil {
		logger.Error("hook: failed to parse input", "error", err)
		fmt.Print("{}")

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
		fmt.Print("{}")
		return
	}

	toolName, _ := data["tool_name"].(string)

	toolInput, _ := data["tool_input"].(map[string]interface{})
	if toolInput == nil {
		fmt.Print("{}")
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
		fmt.Print("{}")
		return
	}

	if isHookDryRun {
		logger.Info("dry-run: would rewrite tool input", "tool", toolName)
	}

	// permissionDecision is omitted so rewriting the args does not also approve
	// the call and remove the operator's permission prompt.
	emitHookOutput(eventPreToolUse, map[string]interface{}{"updatedInput": toolInput})
}

func hookPostToolUse() {
	eng, data, ok := readHookInput()
	if !ok {
		return
	}

	toolOutput, _ := data["tool_output"].(string)
	if toolOutput == "" {
		fmt.Print("{}")
		return
	}

	engDir := engagementDir(eng)

	newIPs := findNewPublicIPs(toolOutput, engDir)
	newDomains := findNewDomains(toolOutput, engDir)

	if isHookDryRun {
		for _, ip := range newIPs {
			logger.Info("dry-run: would auto-map IP", "ip", ip)
		}

		for _, d := range newDomains {
			logger.Info("dry-run: would auto-map domain", "domain", d)
		}
	} else {
		for _, ip := range newIPs {
			if err := addIPMapping(engDir, ip); err != nil {
				logger.Error("failed to add IP mapping", "ip", ip, "error", err)
			}
		}

		for _, domain := range newDomains {
			if err := addDomain(engDir, domain, ""); err != nil {
				logger.Error("failed to add domain", "domain", domain, "error", err)
			}
		}
	}

	mappings := loadMappings(engDir)
	if len(mappings) == 0 {
		fmt.Print("{}")
		return
	}

	rewritten := newRewriter(mappings, directionR2F).rewrite(toolOutput)
	if rewritten == toolOutput {
		fmt.Print("{}")
		return
	}

	if isHookDryRun {
		logger.Info("dry-run: would rewrite tool output", "delta_chars", len(rewritten)-len(toolOutput))
	}

	emitHookOutput(eventPostToolUse, map[string]interface{}{"updatedToolOutput": rewritten})
}

func hookUserPromptSubmit() {
	eng, data, ok := readHookInput()
	if !ok {
		return
	}

	// Claude Code sends the prompt as "prompt"; "prompt_text" is a fallback.
	promptText, _ := data["prompt"].(string)
	if promptText == "" {
		promptText, _ = data["prompt_text"].(string)
	}

	if promptText == "" {
		fmt.Print("{}")
		return
	}

	engDir := engagementDir(eng)

	newDomains := findNewDomains(promptText, engDir)
	newIPs := findNewPublicIPs(promptText, engDir)

	if isHookDryRun {
		for _, d := range newDomains {
			logger.Info("dry-run: would auto-map domain", "domain", d)
		}

		for _, ip := range newIPs {
			logger.Info("dry-run: would auto-map IP", "ip", ip)
		}
	} else {
		for _, domain := range newDomains {
			if err := addDomain(engDir, domain, ""); err != nil {
				logger.Error("failed to add domain", "domain", domain, "error", err)
			}
		}

		for _, ip := range newIPs {
			if err := addIPMapping(engDir, ip); err != nil {
				logger.Error("failed to add IP mapping", "ip", ip, "error", err)
			}
		}
	}

	if len(newDomains) == 0 && len(newIPs) == 0 {
		fmt.Print("{}")
		return
	}

	emitHookOutput(eventUserPromptSubmit, map[string]interface{}{
		"additionalContext": fmt.Sprintf(
			"[tanuki] Auto-mapped %d new domain(s) and %d new IP(s)",
			len(newDomains), len(newIPs),
		),
	})
}

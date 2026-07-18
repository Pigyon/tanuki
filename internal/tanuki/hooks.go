package tanuki

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

var hookDryRun bool

func cmdHook(args []string) {
	if len(args) == 0 {
		fatal("Usage: tanuki hook [--dry-run] <pre-tool-use|post-tool-use|user-prompt-submit>")
	}

	var filtered []string

	for _, a := range args {
		if a == "--dry-run" {
			hookDryRun = true
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

//nolint:funlen // switch statement is long but simple
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

	f2r := newRewriter(mappings, "f2r")
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

	switch toolName {
	case "Bash":
		rewrite("command")
	case "Edit":
		rewrite("file_path")
		rewrite("old_string")
		rewrite("new_string")
	case "Write":
		rewrite("file_path")
		rewrite("content")
	case "WebFetch":
		rewrite("url")
	case "Grep", "Glob":
		rewrite("pattern")
		rewrite("path")
	case "Read":
		rewrite("file_path")
	}

	if changed {
		if hookDryRun {
			logger.Info("dry-run: would rewrite tool input", "tool", toolName)
		}

		_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"updatedInput": toolInput})
	} else {
		fmt.Print("{}")
	}
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

	if hookDryRun {
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

	rewritten := newRewriter(mappings, "r2f").rewrite(toolOutput)
	if rewritten != toolOutput {
		if hookDryRun {
			logger.Info("dry-run: would rewrite tool output", "delta_chars", len(rewritten)-len(toolOutput))
		}

		_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"updatedToolResult": rewritten})
	} else {
		fmt.Print("{}")
	}
}

func hookUserPromptSubmit() {
	eng, data, ok := readHookInput()
	if !ok {
		return
	}

	promptText, _ := data["prompt_text"].(string)
	if promptText == "" {
		fmt.Print("{}")
		return
	}

	engDir := engagementDir(eng)

	newDomains := findNewDomains(promptText, engDir)
	newIPs := findNewPublicIPs(promptText, engDir)

	if hookDryRun {
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

	if len(newDomains) > 0 {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"additionalContext": fmt.Sprintf("[tanuki] Auto-mapped %d new domain(s)", len(newDomains)),
		})
	} else {
		fmt.Print("{}")
	}
}

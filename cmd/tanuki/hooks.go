package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func cmdHook(args []string) {
	if len(args) == 0 {
		fatal("Usage: tanuki hook <pre-tool-use|post-tool-use|user-prompt-submit|session-start>")
	}

	switch args[0] {
	case "pre-tool-use":
		hookPreToolUse()
	case "post-tool-use":
		hookPostToolUse()
	case "user-prompt-submit":
		hookUserPromptSubmit()
	default:
		fatal("Unknown hook: " + args[0])
	}
}

func readHookInput() (string, map[string]interface{}, bool) {
	eng := ensureEngagement()

	input, _ := io.ReadAll(os.Stdin)

	var data map[string]interface{}

	if err := json.Unmarshal(input, &data); err != nil {
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
	case "Bash", "PowerShell":
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

	for _, ip := range findNewPublicIPs(toolOutput) {
		addIPMapping(ip)
	}

	for _, domain := range findNewDomains(toolOutput) {
		addDomain(domain, "")
	}

	engDir := engagementDir(eng)

	mappings := loadMappings(engDir)
	if len(mappings) == 0 {
		fmt.Print("{}")
		return
	}

	if rewritten := newRewriter(mappings, "r2f").rewrite(toolOutput); rewritten != toolOutput {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"updatedToolResult": rewritten})
	} else {
		fmt.Print("{}")
	}
}

func hookUserPromptSubmit() {
	_, data, ok := readHookInput()
	if !ok {
		return
	}

	promptText, _ := data["prompt_text"].(string)
	if promptText == "" {
		fmt.Print("{}")
		return
	}

	newDomains := findNewDomains(promptText)
	for _, domain := range newDomains {
		addDomain(domain, "")
	}

	for _, ip := range findNewPublicIPs(promptText) {
		addIPMapping(ip)
	}

	if len(newDomains) > 0 {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"additionalContext": fmt.Sprintf("[tanuki] Auto-mapped %d new domain(s)", len(newDomains)),
		})
	} else {
		fmt.Print("{}")
	}
}

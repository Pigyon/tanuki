package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "add":
		cmdAdd(os.Args[2:])
	case "add-ip":
		cmdAddIP(os.Args[2:])
	case "map":
		cmdMap()
	case "status":
		cmdStatus()
	case "list":
		cmdList()
	case "activate":
		cmdActivate(os.Args[2:])
	case "test":
		cmdTest(os.Args[2:])
	case "terms":
		cmdTerms()
	case "hook":
		cmdHook(os.Args[2:])
	case "setup":
		cmdSetup()
	case "reset":
		cmdReset(os.Args[2:])
	case "proxy":
		runProxy()
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`tanuki - Transparent anonymization proxy for AI-assisted security testing

Proxy (runs inside Docker container):
  tanuki proxy                             Start the proxy server

Host CLI (run on your machine to manage engagements):
  tanuki setup                             Configure Claude Code hooks (auto-creates engagement)
  tanuki init <name> [--fiction-org NAME]   Create named engagement (optional)
  tanuki add <domain> [--fiction-org NAME]  Pre-seed a target domain (optional, auto-detected)
  tanuki add-ip <ip>                       Pre-seed an IP mapping (optional, auto-detected)
  tanuki map                               Show current mapping table
  tanuki status                            Show engagement info
  tanuki list                              List engagements
  tanuki activate <name>                   Switch engagement
  tanuki test <text>                       Test rewriting on sample text
  tanuki terms                             Show terminology mappings
  tanuki reset [--data]                    Remove hooks and CLAUDE.md (--data also wipes engagements)
  tanuki help                              Show this help

Workflow (zero-config):
  1. docker compose up -d
  2. tanuki setup
  3. ANTHROPIC_BASE_URL=http://localhost:18080 claude

Domains, emails, and IPs are auto-detected from your conversation.
Use 'tanuki init' and 'tanuki add' only if you need custom fiction org names.
`)
}

func cmdTest(args []string) {
	if len(args) == 0 {
		fatal("Usage: tanuki test <text>")
	}

	text := strings.Join(args, " ")
	eng := requireEngagement()
	mappings := loadMappings(engagementDir(eng))

	r2f := newRewriter(mappings, "r2f")
	f2r := newRewriter(mappings, "f2r")

	fmt.Println("=== Real -> Fiction ===")
	fmt.Println(r2f.rewrite(text))
	fmt.Println()
	fmt.Println("=== Round-trip ===")
	fmt.Println(f2r.rewrite(r2f.rewrite(text)))
}

func cmdTerms() {
	fmt.Printf("%-35s %s\n", "PENTEST TERM", "DEV TERM")
	fmt.Printf("%-35s %s\n", strings.Repeat("-", 35), strings.Repeat("-", 35))

	for _, t := range terminology {
		fmt.Printf("%-35s %s\n", t.From, t.To)
	}
}

func cmdSetup() {
	eng := ensureEngagement()
	engDir := engagementDir(eng)

	tanukiBin, _ := os.Executable()
	if tanukiBin == "" {
		tanukiBin = "tanuki"
	}

	configureHooks(tanukiBin)
	generateClaudeMD(engDir)

	fmt.Println("=== Tanuki Setup Complete ===")
	fmt.Printf("Engagement:  %s\n", eng)
	fmt.Printf("Mappings:    %d\n", len(loadMappings(engDir)))
	fmt.Println()
	fmt.Println("Hooks configured in .claude/settings.json")
	fmt.Println("CLAUDE.md generated in current directory")
	fmt.Println()
	fmt.Printf("Start Claude Code with:\n  ANTHROPIC_BASE_URL=http://localhost:%s claude\n", getProxyPort())
}

// --- Shared utilities ---

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func dataDir() string      { return envOr("TANUKI_DATA", "data") }
func getProxyPort() string { return envOr("TANUKI_PROXY_PORT", "18080") }
func portStart() string    { return envOr("TANUKI_PORT_START", "9000") }

func getEngagement() string {
	data, err := os.ReadFile(filepath.Join(dataDir(), "current_engagement"))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

func requireEngagement() string {
	eng := getEngagement()
	if eng == "" {
		fatal("no active engagement, run: tanuki init <name>")
	}

	return eng
}

func ensureEngagement() string {
	eng := getEngagement()
	if eng != "" && fileExists(engagementDir(eng)) {
		return eng
	}

	name := "default"
	engDir := engagementDir(name)

	if !fileExists(engDir) {
		if err := os.MkdirAll(engDir, 0755); err != nil {
			fatal(fmt.Sprintf("creating directory %s: %v", engDir, err))
		}

		writeFileContent(filepath.Join(engDir, "fiction_org"), "DEVTARGET")
		writeFileContent(filepath.Join(engDir, "port_counter"), portStart())
		writeFileContent(filepath.Join(engDir, "ip_counter"), "2")
		writeFileContent(filepath.Join(engDir, "mappings.conf"),
			"# Tanuki engagement mappings\n# Format: TYPE|REAL_VALUE|FICTION_VALUE\n")
	}

	writeFileContent(filepath.Join(dataDir(), "current_engagement"), name)

	return name
}

func engagementDir(eng string) string {
	return filepath.Join(dataDir(), eng)
}

func readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

func writeFileContent(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		fatal(fmt.Sprintf("writing %s: %v", path, err))
	}
}

func appendToFile(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fatal(fmt.Sprintf("opening %s: %v", path, err))
	}

	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "[tanuki] ERROR: %s\n", msg)
	os.Exit(1)
}

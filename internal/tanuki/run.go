package tanuki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var lastKnownEngagement atomic.Value

// Run is the CLI entrypoint: it parses os.Args and dispatches to the
// matching command, exiting the process on error.
func Run() {
	initLogger()

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
	case "add-rule":
		cmdAddRule(os.Args[2:])
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
	case "export":
		cmdExport(os.Args[2:])
	case "import":
		cmdImport(os.Args[2:])
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

Commands:
  proxy                             Start the proxy server (container entrypoint)
  setup                             Configure Claude Code hooks (auto-creates engagement)
  init <name> [--fiction-org NAME]   Create named engagement
  add <domain> [--fiction-org NAME]  Pre-seed a target domain
  add-ip <ip>                       Pre-seed an IP mapping
  add-rule <real> <fiction>          Add a custom rewrite rule
  map                               Show current mapping table
  status                            Show engagement info
  list                              List engagements
  activate <name>                   Switch engagement
  test <text>                       Test rewriting on sample text
  terms                             Show terminology mappings
  export [name]                     Export engagement as JSON
  import <file.json>                Import engagement from JSON
  reset [--data]                    Remove hooks (--data also wipes engagements)
  help                              Show this help

Workflow:
  1. docker compose up -d
  2. claude
`)
}

func cmdTest(args []string) {
	if len(args) == 0 {
		fatal("Usage: tanuki test <text>")
	}

	text := strings.Join(args, " ")

	eng, err := requireEngagement()
	if err != nil {
		fatal(err.Error())
	}

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
	eng, err := ensureEngagement()
	if err != nil {
		fatal(err.Error())
	}

	engDir := engagementDir(eng)

	if err := configureHooks(); err != nil {
		fatal(err.Error())
	}

	if err := generateClaudeMD(engDir); err != nil {
		fatal(err.Error())
	}

	fmt.Println("=== Tanuki Setup Complete ===")
	fmt.Printf("Engagement:  %s\n", eng)
	fmt.Printf("Mappings:    %d\n", len(loadMappings(engDir)))
	fmt.Println()
	fmt.Println("Hooks + env configured in .claude/settings.json")
	fmt.Println("CLAUDE.md generated in .claude/CLAUDE.md")
	fmt.Println()
	fmt.Println("Start Claude Code with:\n  claude")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func dataDir() string      { return envOr("TANUKI_DATA", "data") }
func getProxyPort() string { return envOr("TANUKI_PROXY_PORT", "18080") }
func portStart() string    { return envOr("TANUKI_PORT_START", "9000") }

// getEngagement reads the active engagement, falling back to the last
// known value when the file is empty or missing (guards against a
// concurrent truncate-then-write race exposing an empty read).
func getEngagement() string {
	data, err := os.ReadFile(filepath.Join(dataDir(), "current_engagement"))
	if err == nil {
		if eng := strings.TrimSpace(string(data)); eng != "" {
			lastKnownEngagement.Store(eng)
			return eng
		}
	}

	if v := lastKnownEngagement.Load(); v != nil {
		return v.(string)
	}

	return ""
}

func requireEngagement() (string, error) {
	eng := getEngagement()
	if eng == "" {
		return "", fmt.Errorf("no active engagement, run: tanuki init <name>")
	}

	return eng, nil
}

func ensureEngagement() (string, error) {
	eng := getEngagement()
	if eng != "" && fileExists(engagementDir(eng)) {
		return eng, nil
	}

	name := "default"
	engDir := engagementDir(name)

	if !fileExists(engDir) {
		if err := os.MkdirAll(engDir, 0755); err != nil {
			return "", fmt.Errorf("creating directory %s: %w", engDir, err)
		}

		if err := writeFileContent(filepath.Join(engDir, "fiction_org"), "DEVTARGET"); err != nil {
			return "", err
		}

		if err := writeFileContent(filepath.Join(engDir, "port_counter"), portStart()); err != nil {
			return "", err
		}

		if err := writeFileContent(filepath.Join(engDir, "ip_counter"), "2"); err != nil {
			return "", err
		}

		if err := writeFileContent(
			filepath.Join(engDir, "mappings.conf"),
			"# Tanuki engagement mappings\n# Format: TYPE|REAL_VALUE|FICTION_VALUE\n",
		); err != nil {
			return "", err
		}
	}

	if err := writeFileContent(filepath.Join(dataDir(), "current_engagement"), name); err != nil {
		return "", err
	}

	return name, nil
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

func writeFileContent(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

func appendToFile(path, line string) error {
	unlock := acquireFileLock(path)
	defer unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	defer f.Close()

	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("writing to %s: %w", path, err)
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fatal(msg string) {
	if logger != nil {
		logger.Error(msg)
	} else {
		fmt.Fprintf(os.Stderr, "[tanuki] ERROR: %s\n", msg)
	}

	os.Exit(1)
}

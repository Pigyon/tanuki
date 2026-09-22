package tanuki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// exitUsage is for bad invocation; exitError for runtime failures. Hooks never
// reach exitUsage, so a crash can't exit 2 and block a tool in Claude Code.
const (
	exitError = 1
	exitUsage = 2
)

var lastKnownEngagement atomic.Value

// commands maps each command name to its handler, which receives the args
// following the command name.
var commands = map[string]func(args []string){
	"init":      cmdInit,
	"add":       cmdAdd,
	"add-ip":    cmdAddIP,
	"add-rule":  cmdAddRule,
	"map":       func([]string) { cmdMap() },
	"status":    func([]string) { cmdStatus() },
	"list":      func([]string) { cmdList() },
	"activate":  cmdActivate,
	"test":      cmdTest,
	"verify":    cmdVerify,
	"terms":     func([]string) { cmdTerms() },
	"hook":      cmdHook,
	"setup":     func([]string) { cmdSetup() },
	"export":    cmdExport,
	"import":    cmdImport,
	"reset":     cmdReset,
	"proxy":     func([]string) { runProxy() },
	"version":   func([]string) { cmdVersion() },
	"--version": func([]string) { cmdVersion() },
	"-v":        func([]string) { cmdVersion() },
	"help":      func([]string) { usage() },
	"--help":    func([]string) { usage() },
	"-h":        func([]string) { usage() },
}

// Run is the CLI entrypoint: it parses os.Args and dispatches to the
// matching command, exiting the process on error.
func Run() {
	initLogger()

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}

	handler, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}

	handler(os.Args[2:])
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
  verify [file...|-]                Check the mappings hold; given files, "-",
                                    or "< file", list unmapped targets in them
  terms                             Show terminology mappings
  export [name]                     Export engagement as JSON
  import <file.json>                Import engagement from JSON
  reset [--data]                    Remove hooks (--data also wipes engagements)
  version                           Show version and build info
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
	r2f := newRewriter(mappings, directionR2F)
	f2r := newRewriter(mappings, directionF2R)

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

func dataDir() string   { return envOr("TANUKI_DATA", "data") }
func proxyPort() string { return envOr("TANUKI_PROXY_PORT", "18080") }
func portStart() string { return envOr("TANUKI_PORT_START", "9000") }

// currentEngagement reads the active engagement, falling back to lastKnown on an
// empty/missing read. It validates the (user-writable) name and never fatals.
func currentEngagement() string {
	data, err := os.ReadFile(filepath.Join(dataDir(), "current_engagement"))
	if err == nil {
		if eng := strings.TrimSpace(string(data)); eng != "" && validateEngagementName(eng) == nil {
			lastKnownEngagement.Store(eng)
			return eng
		}
	}

	if v := lastKnownEngagement.Load(); v != nil {
		if eng, ok := v.(string); ok {
			return eng
		}
	}

	return ""
}

var errNoEngagement = errors.New("no active engagement, run: tanuki init <name>")

func requireEngagement() (string, error) {
	eng := currentEngagement()
	if eng == "" {
		return "", errNoEngagement
	}

	return eng, nil
}

func ensureEngagement() (string, error) {
	eng := currentEngagement()
	if eng != "" && fileExists(engagementDir(eng)) {
		return eng, nil
	}

	name := "default"

	engDir := engagementDir(name)
	if !fileExists(engDir) {
		if err := seedEngagement(engDir, "DEVTARGET"); err != nil {
			return "", err
		}
	}

	if err := writeFileContent(filepath.Join(dataDir(), "current_engagement"), name); err != nil {
		return "", err
	}

	return name, nil
}

// seedEngagement creates engDir and writes the initial counter and mapping
// files for a new engagement.
func seedEngagement(engDir, fictionOrg string) error {
	if err := os.MkdirAll(engDir, 0o750); err != nil {
		return fmt.Errorf("creating directory %s: %w", engDir, err)
	}

	files := []struct{ name, content string }{
		{"fiction_org", fictionOrg},
		{"port_counter", portStart()},
		{"ip_counter", "2"},
		{"org_counter", "1"},
		{"mappings.conf", "# Tanuki engagement mappings\n# Format: TYPE|REAL_VALUE|FICTION_VALUE\n"},
	}

	for _, f := range files {
		if err := writeFileContent(filepath.Join(engDir, f.name), f.content); err != nil {
			return err
		}
	}

	return nil
}

// engagementDir is a pure path join with no validation; callers with an
// untrusted name must run it through validateEngagementName first.
func engagementDir(eng string) string {
	return filepath.Join(dataDir(), eng)
}

// validateEngagementName rejects names that would escape the data directory or
// are unsafe as a single path component (CLI args, imports, the active marker).
func validateEngagementName(name string) error {
	if name == "" {
		return errors.New("engagement name is empty")
	}

	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid engagement name %q", name)
	}

	return nil
}

func readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

// writeFileContent writes /data engagement state 0600: it holds the mapping
// table, read only by tanuki, so it must not be world-readable.
func writeFileContent(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// writeSharedFile writes host-readable .claude/ config (0644). The container
// writes as root over a bind mount, so 0600 would hide it on native-Linux Docker.
func writeSharedFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

func appendToFile(path, line string) error {
	unlock := acquireFileLock(path)
	defer unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()

		return fmt.Errorf("writing to %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
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

	os.Exit(exitError)
}

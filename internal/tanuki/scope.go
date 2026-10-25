package tanuki

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const scopeFile = "scope.conf"

// scope is the set of domains an engagement is actually testing, as a bug
// bounty program publishes it.
//
// It decides how much fiction a detected domain earns. A domain in scope is a
// target: it gets the derived org, email, path and cloud mappings, so the org
// name is rewritten wherever it appears. A domain merely seen in traffic gets
// a hostname mapping and nothing else, because an org rule for it rewrites an
// everyday word everywhere -- one docs.python.org in tool output used to turn
// "written in Python" into "written in DEVTARGET2".
//
// An empty scope means every detected domain is treated as a target, which is
// how an engagement behaves until a scope is set.
type scope []string

// parseScope reads one pattern per line, ignoring blanks and comments. A
// leading "*." is dropped: "*.acme.com" and "acme.com" both cover the apex and
// everything under it, which is what a program means by either.
func parseScope(data string) scope {
	var s scope

	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if p := normalizeScopePattern(line); p != "" && !slices.Contains(s, p) {
			s = append(s, p)
		}
	}

	return s
}

func normalizeScopePattern(pattern string) string {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	pattern = strings.TrimPrefix(pattern, "*.")

	return strings.Trim(pattern, ".")
}

func loadScope(engDir string) scope {
	return parseScope(readFileContent(filepath.Join(engDir, scopeFile)))
}

// covers reports whether domain is the pattern or sits under it.
func (s scope) covers(domain string) bool {
	domain = strings.ToLower(domain)

	for _, p := range s {
		if domain == p || strings.HasSuffix(domain, "."+p) {
			return true
		}
	}

	return false
}

// isTarget reports whether a detected domain earns the derived mappings. With
// no scope set, every domain does.
func (s scope) isTarget(domain string) bool {
	return len(s) == 0 || s.covers(domain)
}

// addToScope appends a pattern unless it is already covered, and reports
// whether it did.
func addToScope(engDir, pattern string) (bool, error) {
	p := normalizeScopePattern(pattern)
	if p == "" {
		return false, fmt.Errorf("invalid scope pattern: %q", pattern)
	}

	if strings.ContainsAny(p, "|\r\n") {
		return false, fmt.Errorf("invalid scope pattern: %q", pattern)
	}

	unlock := lockAllocation(engDir)
	defer unlock()

	if slices.Contains(loadScope(engDir), p) {
		return false, nil
	}

	return true, appendToFile(filepath.Join(engDir, scopeFile), p)
}

// removeFromScope drops a pattern and reports whether it was there.
func removeFromScope(engDir, pattern string) (bool, error) {
	p := normalizeScopePattern(pattern)

	unlock := lockAllocation(engDir)
	defer unlock()

	current := loadScope(engDir)
	if !slices.Contains(current, p) {
		return false, nil
	}

	kept := slices.DeleteFunc(slices.Clone(current), func(e string) bool { return e == p })

	return true, writeScope(engDir, kept)
}

func writeScope(engDir string, s scope) error {
	content := "# Tanuki engagement scope: one domain per line\n"
	if len(s) > 0 {
		content += strings.Join(s, "\n") + "\n"
	}

	return os.WriteFile(filepath.Join(engDir, scopeFile), []byte(content), 0o600)
}

// seedEnvScope adds the comma-separated patterns in TANUKI_SCOPE.
func seedEnvScope(engDir, patterns string) error {
	for _, p := range strings.Split(patterns, ",") {
		if strings.TrimSpace(p) == "" {
			continue
		}

		added, err := addToScope(engDir, p)
		if err != nil {
			return err
		}

		if added {
			logger.Info("added scope entry from env", "pattern", normalizeScopePattern(p))
		}
	}

	return nil
}

func cmdScope(args []string) {
	eng, err := requireEngagement()
	if err != nil {
		fatal(err.Error())
	}

	engDir := engagementDir(eng)

	if len(args) == 0 {
		showScope(engDir)
		return
	}

	switch args[0] {
	case "list":
		showScope(engDir)
	case "add":
		scopeChange(engDir, args[1:], addToScope, "Added to scope", "Already in scope")
	case "rm":
		scopeChange(engDir, args[1:], removeFromScope, "Removed from scope", "Not in scope")
	default:
		fatal("Usage: tanuki scope [list | add <domain> | rm <domain>]")
	}
}

func scopeChange(engDir string, args []string, apply func(string, string) (bool, error), done, noop string) {
	if len(args) == 0 {
		fatal("Usage: tanuki scope add|rm <domain>")
	}

	changed, err := apply(engDir, args[0])
	if err != nil {
		fatal(err.Error())
	}

	label := noop
	if changed {
		label = done
	}

	fmt.Printf("[tanuki] %s: %s\n", label, normalizeScopePattern(args[0]))
}

func showScope(engDir string) {
	current := loadScope(engDir)
	if len(current) == 0 {
		fmt.Println("Scope is empty: every detected domain is treated as a target.")
		fmt.Println("Add one with: tanuki scope add <domain>")

		return
	}

	fmt.Println("In scope (targets; everything else detected is mapped by hostname only):")

	for _, p := range current {
		fmt.Printf("  %s and *.%s\n", p, p)
	}
}

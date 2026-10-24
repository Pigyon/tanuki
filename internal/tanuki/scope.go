package tanuki

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const scopeFile = "scope.conf"

// scope is the set of domains an engagement is actually testing, as a bug
// bounty program publishes it.
//
// A domain in scope is a target and earns the derived org, email, path and
// cloud mappings. A domain merely seen gets a hostname mapping and nothing
// else, because an org rule rewrites an everyday word everywhere: one
// docs.python.org in tool output turned "written in Python" into "written in
// DEVTARGET2". An empty scope treats every detected domain as a target.
type scope []string

// parseScope reads one pattern per line, ignoring blanks and comments. A
// leading "*." is dropped: either spelling covers the apex and everything
// under it, which is what a program means by both.
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

// isTarget reports whether a detected domain earns the derived mappings.
//
// With no scope set, a common TLD stands in for one: that is every domain
// detection used to find, so an engagement without a scope behaves as before.
// A domain recognised only by its context -- any real TLD after "://", after
// "@" or before a path -- is hidden but gets no org rule, because an org rule
// rewrites a bare word everywhere and that guess is too weak to spend one on.
// "tanuki add" and an explicit scope entry both still map in full.
func (s scope) isTarget(domain string) bool {
	if len(s) == 0 {
		return hasCommonTLD(domain) && worthAnOrgRule(extractOrgName(domain))
	}

	return s.covers(domain)
}

// addToScope appends a pattern unless it is already covered, and reports
// whether it did.
func addToScope(engDir, pattern string) (bool, error) {
	p := normalizeScopePattern(pattern)
	if !scopePatternRegex.MatchString(p) {
		return false, fmt.Errorf("invalid scope pattern %q: expected a domain such as example.com or *.example.com", pattern)
	}

	added, err := appendScopePattern(engDir, p)
	if err != nil || !added {
		return added, err
	}

	// Promotion runs outside the lock: addDomain takes the same one.
	return true, promoteToTargets(engDir, p)
}

// scopePatternRegex is a hostname, once the leading "*." is gone. Without it a
// pasted URL or a typo was stored as a pattern that covers() can never match,
// and because any non-empty scope makes everything outside it host-only, a
// scope of nothing but typos quietly downgraded every target.
var scopePatternRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func appendScopePattern(engDir, p string) (bool, error) {
	unlock, err := lockAllocation(engDir)
	if err != nil {
		return false, err
	}

	defer unlock()

	if slices.Contains(loadScope(engDir), p) {
		return false, nil
	}

	return true, appendToFile(filepath.Join(engDir, scopeFile), p)
}

// promoteToTargets gives the derived mappings to domains that were mapped by
// hostname alone before this pattern said they were targets. Detection never
// revisits a domain it has already mapped, so without this a domain mapped
// before its scope entry stayed host-only for the rest of the engagement.
func promoteToTargets(engDir, pattern string) error {
	covering := scope{pattern}

	var err error

	for _, m := range loadMappings(engDir) {
		if m.Type != mapDomain || !covering.covers(m.Real) {
			continue
		}

		if addErr := addDomain(engDir, m.Real, ""); addErr != nil {
			err = errors.Join(err, fmt.Errorf("promoting %s: %w", m.Real, addErr))
		}
	}

	proxyCache.invalidate()

	return err
}

// removeFromScope drops a pattern and reports whether it was there.
func removeFromScope(engDir, pattern string) (bool, error) {
	p := normalizeScopePattern(pattern)

	unlock, err := lockAllocation(engDir)
	if err != nil {
		return false, err
	}

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

	return writeFileContent(filepath.Join(engDir, scopeFile), content)
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
		warnOrphanedDerived(engDir, args[1:])
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

// warnOrphanedDerived says which derived mappings a removed pattern leaves
// behind. They are not deleted: the model has already been shown those
// fictions, and dropping them would leave its earlier replies unreversible.
func warnOrphanedDerived(engDir string, args []string) {
	if len(args) == 0 {
		return
	}

	removed := scope{normalizeScopePattern(args[0])}
	mappings := loadMappings(engDir)

	// The org names belonging to domains the pattern covered.
	orgs := map[string]bool{}

	for _, m := range mappings {
		if m.Type == mapDomain && removed.covers(m.Real) {
			orgs[strings.ToLower(extractOrgName(m.Real))] = true
		}
	}

	var orphaned []string

	for _, m := range mappings {
		if m.Type == mapOrg && orgs[m.Real] {
			orphaned = append(orphaned, m.Real)
		}
	}

	if len(orphaned) == 0 {
		return
	}

	fmt.Println("[tanuki] Mappings already created for it are kept, so earlier replies still reverse:")

	for _, name := range orphaned {
		fmt.Printf("  org %q is still rewritten\n", name)
	}

	fmt.Println("[tanuki] Start a new engagement if it must stop being rewritten.")
}

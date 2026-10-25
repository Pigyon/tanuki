package tanuki

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Policy for a real value that is still present after anonymization.
const (
	onLeakRefuse = "refuse"
	onLeakWarn   = "warn"
)

// onLeakPolicy reports what to do when anonymization leaves a real value
// behind. Refusing is the default; warn exists so an operator whose traffic
// trips a false positive can keep working while it is diagnosed.
func onLeakPolicy() string {
	if envOr("TANUKI_ON_LEAK", onLeakRefuse) == onLeakWarn {
		return onLeakWarn
	}

	return onLeakRefuse
}

// builtinIgnored are never targets, yet Claude Code puts them in the body of
// nearly every request as part of its own operation, not the engagement: the
// provider's own domains and the JSON Schema dialect URL in every tool schema,
// plus the developer-platform domains it surfaces (git remotes and module
// paths on GitHub, the docs/search connectors on Google). Anonymizing them
// hides nothing and polluted the table -- mapping "claude" as an org rewrote
// the model name, and "github"/"google" rewrote those words everywhere.
//
// github.com and google.com can be a real target, so they are only skipped by
// auto-detection; "tanuki add github.com" still maps one explicitly when the
// engagement is against it.
var builtinIgnored = []string{
	"anthropic.com", "claude.ai", "claude.com", "json-schema.org",
	"github.com", "githubusercontent.com", "google.com", "googleapis.com",
}

// userEmailRegex pulls the operator's own address out of the "# userEmail"
// context block Claude Code injects. Its domain identifies the operator, not a
// target, so it is ignored -- discovered at runtime from the request so no
// address is ever written into tanuki. Anchored to the marker so an address
// quoted elsewhere ("the admin's email is admin@target.com") is not mistaken
// for the operator's and left unmapped.
var userEmailRegex = regexp.MustCompile(`(?is)#\s*userEmail\b.{0,200}?([a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,})`)

// operatorIgnoredDomains returns the base domain of the operator's own email,
// if Claude Code's userEmail block is present, so a single request can ignore
// it without it ever becoming a mapping. Empty for any other client or body.
//
// It walks the decoded body and refuses to read the marker out of a
// tool_result block. That block carries whatever the target returned -- an HTTP
// response, command output, a fetched page -- so trusting it would let a target
// forge "# userEmail ...@real-target.com" to exempt itself from anonymization
// and leak. The genuine block is a Claude Code system reminder, never tool
// output, so skipping tool_result loses nothing.
func operatorIgnoredDomains(doc any) []string {
	seen := map[string]bool{}
	out := []string{}

	var walk func(v any)

	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if t[typeKey] == "tool_result" {
				return
			}

			for _, val := range t {
				walk(val)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		case string:
			for _, m := range userEmailRegex.FindAllStringSubmatch(t, -1) {
				at := strings.LastIndex(m[1], "@")
				if at < 0 {
					continue
				}

				base := extractBaseDomain(strings.ToLower(m[1][at+1:]))
				if base != "" && !seen[base] {
					seen[base] = true
					out = append(out, base)
				}
			}
		}
	}

	walk(doc)

	return out
}

// isIgnoredDomain reports whether domain, the base domain it belongs to, or
// any parent of it is built in, listed in TANUKI_IGNORE (comma-separated), or
// in extra (ignores derived from this one request, such as the operator's own
// email domain). Ignored domains are neither auto-mapped nor reported by the
// residual check; "tanuki add" still maps one explicitly.
func isIgnoredDomain(domain string, extra ...string) bool {
	domain = strings.ToLower(domain)
	base := extractBaseDomain(domain)

	for _, ig := range slices.Concat(builtinIgnored, strings.Split(os.Getenv("TANUKI_IGNORE"), ","), extra) {
		ig = strings.ToLower(strings.TrimSpace(ig))
		if ig == "" {
			continue
		}

		if ig == domain || ig == base || strings.HasSuffix(domain, "."+ig) {
			return true
		}
	}

	return false
}

// mapTargets gives every unmapped domain, public IP and AWS account ID in
// texts a fiction and reports how many it mapped. Every candidate list is
// collected before anything is written, so no pass sees another's mappings.
func mapTargets(engDir string, extraIgnored []string, texts ...string) (int, error) {
	found := findNewTargets(engDir, texts, extraIgnored...)
	sc := loadScope(engDir)

	var err error

	for _, domain := range found.domains {
		var addErr error

		if sc.isTarget(domain) {
			addErr = addDomain(engDir, domain, "")
		} else {
			addErr = addHostOnlyDomain(engDir, domain)
		}

		if addErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping domain %s: %w", domain, addErr))
		}
	}

	for _, ip := range found.ips {
		if addErr := addIPMapping(engDir, ip); addErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping IP %s: %w", ip, addErr))
		}
	}

	for _, id := range found.accounts {
		if addErr := addAccountMapping(engDir, id); addErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping account %s: %w", id, addErr))
		}
	}

	return len(found.domains) + len(found.ips) + len(found.accounts), err
}

// residualTargets returns the real-looking values still present in text. It
// applies the same recognisers and the same filters as detection, so every
// value it reports is one that detection should have mapped and the rewriter
// should have replaced. Against a body that has been through both, the result
// is empty; anything else means a value is about to leave unanonymized.
//
// Fiction values are not recognised as targets -- "localhost:9000" has no dot,
// "devtarget.local" has no common TLD and the loopback IPs are filtered as
// private -- so tanuki's own output never trips it.
func residualTargets(extraIgnored []string, texts ...string) []string {
	seen := make(map[string]bool)
	found := []string{}

	for _, text := range texts {
		s := targetsIn(text)

		for _, v := range slices.Concat(s.domains, s.ips, s.accounts) {
			if seen[v] || isIgnoredDomain(v, extraIgnored...) {
				continue
			}

			seen[v] = true

			found = append(found, v)
		}
	}

	return found
}

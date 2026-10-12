package tanuki

import (
	"errors"
	"fmt"
	"os"
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

// builtinIgnored are never targets, and Claude Code sends them on every
// request: the provider's own domains, named in its system prompt, and the
// JSON Schema dialect URL in every tool schema. Anonymizing them hides nothing
// and mapped "claude" as an org, which rewrote the model name.
var builtinIgnored = []string{"anthropic.com", "claude.ai", "claude.com", "json-schema.org"}

// isIgnoredDomain reports whether domain, or the base domain it belongs to, is
// built in or listed in TANUKI_IGNORE (comma-separated). Ignored domains are
// neither auto-mapped nor reported by the residual check; "tanuki add" still
// maps one explicitly.
func isIgnoredDomain(domain string) bool {
	domain = strings.ToLower(domain)
	base := extractBaseDomain(domain)

	for _, ig := range slices.Concat(builtinIgnored, strings.Split(os.Getenv("TANUKI_IGNORE"), ",")) {
		ig = strings.ToLower(strings.TrimSpace(ig))
		if ig != "" && (ig == domain || ig == base) {
			return true
		}
	}

	return false
}

// mapTargets gives every unmapped domain and public IP in texts a fiction and
// reports how many of each it found. Both candidate lists are collected before
// anything is written, so neither pass sees the other's mappings.
func mapTargets(engDir string, texts ...string) (domains, ips int, err error) {
	newDomains, newIPs := findNewTargets(engDir, texts)

	for _, domain := range newDomains {
		if addErr := addDomain(engDir, domain, ""); addErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping domain %s: %w", domain, addErr))
		}
	}

	for _, ip := range newIPs {
		if addErr := addIPMapping(engDir, ip); addErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping IP %s: %w", ip, addErr))
		}
	}

	return len(newDomains), len(newIPs), err
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
func residualTargets(texts ...string) []string {
	seen := make(map[string]bool)
	found := []string{}

	for _, text := range texts {
		domains, ips := targetsIn(text)

		for _, v := range slices.Concat(domains, ips) {
			if seen[v] || isIgnoredDomain(v) {
				continue
			}

			seen[v] = true

			found = append(found, v)
		}
	}

	return found
}

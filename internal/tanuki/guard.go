package tanuki

import (
	"os"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
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
	// Package registries and documentation hosts: they appear in go.mod,
	// package.json, imports and docs links on nearly every coding turn.
	"golang.org", "go.dev", "gopkg.in", "pkg.go.dev",
	"npmjs.com", "npmjs.org", "pypi.org", "crates.io", "docs.rs", "rubygems.org",
	"stackoverflow.com", "mozilla.org", "w3.org", "readthedocs.io",
	"nuget.org", "packagist.org", "maven.apache.org", "apache.org",
	"nodejs.org", "python.org", "rust-lang.org", "helm.sh", "k8s.io",
	"schemas.android.com", "oracle.com", "letsencrypt.org",
	// Container registries and forges, which appear in every Dockerfile,
	// compose file and clone URL.
	"docker.io", "ghcr.io", "quay.io", "gcr.io", "registry.k8s.io",
	"gitlab.com", "bitbucket.org",
	// Reserved for documentation by RFC 2606; never a real target.
	"example.com", "example.org", "example.net",
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
// Only the places Claude Code writes that block are read: the system prompt,
// and user-role content that is not a tool_result. Everything else carries
// text a target may have chosen. A page saying "# userEmail admin@target.com"
// is summarised by the model into an assistant block, or returned through a
// tool_result, and reading the marker there let the target exempt itself from
// anonymization for the rest of the session -- domain, subdomains and all.
func operatorIgnoredDomains(doc any) []string {
	body, _ := doc.(map[string]any)
	if body == nil {
		return nil
	}

	found := &operatorEmails{seen: map[string]bool{}}

	found.scan(body["system"])

	// Only the first user message. Claude Code injects the account context at
	// the start of the conversation and resends it every turn, while later
	// user messages carry @-mentioned file contents and pastes -- target
	// material on an engagement, and a place to plant the marker.
	messages, _ := body["messages"].([]any)
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		if msg == nil || msg["role"] != "user" {
			continue
		}

		found.scan(msg["content"])

		break
	}

	return found.domains
}

// operatorEmails collects the base domains of the operator addresses found in
// the parts of a body that Claude Code, not a target, writes.
type operatorEmails struct {
	seen    map[string]bool
	domains []string
}

func (o *operatorEmails) scan(v any) {
	switch t := v.(type) {
	case map[string]any:
		if t[typeKey] == "tool_result" {
			return
		}

		for _, val := range t {
			o.scan(val)
		}
	case []any:
		for _, item := range t {
			o.scan(item)
		}
	case string:
		o.add(t)
	}
}

func (o *operatorEmails) add(text string) {
	for _, m := range userEmailRegex.FindAllStringSubmatch(text, -1) {
		at := strings.LastIndex(m[1], "@")
		if at < 0 {
			continue
		}

		base := extractBaseDomain(strings.ToLower(m[1][at+1:]))

		// A bare public suffix is not an address anyone owns: extractBaseDomain
		// hands "co.uk" straight back, and ignoring that exempts every domain
		// under it. One forged "# userEmail admin@co.uk" turned off
		// anonymization for all of .co.uk.
		if suffix, _ := publicsuffix.PublicSuffix(base); base == "" || base == suffix {
			continue
		}

		if !o.seen[base] {
			o.seen[base] = true
			o.domains = append(o.domains, base)
		}
	}
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

	mapped := 0

	for _, domain := range found.domains {
		mapped += logMapFailure("domain", domain, mapOneDomain(engDir, sc, domain))
	}

	for _, ip := range found.ips {
		mapped += logMapFailure("ip", ip, addIPMapping(engDir, ip))
	}

	for _, id := range found.accounts {
		mapped += logMapFailure("account", id, addAccountMapping(engDir, id))
	}

	return mapped, nil
}

// logMapFailure records that one value could not be mapped and reports whether
// it was, so one bad value cannot refuse the whole body.
//
// The error is logged, never returned: it names the value, and the refusal it
// would cause is shown to the client, which folds the text into its next
// prompt and sends the real value upstream that way. A value that truly is in
// the body and unmapped is still caught by the gate, which reports counts.
func logMapFailure(kind, value string, err error) int {
	if err == nil {
		return 1
	}

	logger.Warn("could not map a detected value", "kind", kind, "value", value, "error", err)

	return 0
}

// mapOneDomain maps a detected domain, falling back to a hostname-only
// mapping when the derived set conflicts with something already in the table.
// A refusal here reaches anonymizeUpstreamBody and refuses the body, and since
// Claude Code resends history that would refuse the conversation for good --
// so the hostname, which is what actually hides the target, is worth keeping
// even when the org name cannot be.
func mapOneDomain(engDir string, sc scope, domain string) error {
	if !sc.isTarget(domain) {
		return addHostOnlyDomain(engDir, domain)
	}

	err := addDomain(engDir, domain, "")
	if err == nil {
		return nil
	}

	logger.Warn("mapping the target in full failed; mapping its hostname only",
		"domain", domain, "error", err)

	return addHostOnlyDomain(engDir, domain)
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

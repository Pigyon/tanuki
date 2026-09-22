package tanuki

import (
	"errors"
	"fmt"
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

// mapTargets gives every unmapped domain and public IP in text a fiction and
// reports how many of each it found. Both candidate lists are collected before
// anything is written, so neither pass sees the other's mappings.
//
// dryRun logs the allocations it would make and writes nothing.
func mapTargets(engDir, text string, dryRun bool) (domains, ips int, err error) {
	newDomains := findNewDomains(text, engDir)
	newIPs := findNewPublicIPs(text, engDir)

	for _, domain := range newDomains {
		if dryRun {
			logger.Info("dry-run: would auto-map domain", "domain", domain)
			continue
		}

		if addErr := addDomain(engDir, domain, ""); addErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping domain %s: %w", domain, addErr))
		}
	}

	for _, ip := range newIPs {
		if dryRun {
			logger.Info("dry-run: would auto-map IP", "ip", ip)
			continue
		}

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
func residualTargets(text string) []string {
	seen := make(map[string]bool)
	found := []string{}

	for _, raw := range domainRegex.FindAllString(text, -1) {
		domain := strings.ToLower(raw)
		if !hasCommonTLD(domain) || seen[domain] {
			continue
		}

		seen[domain] = true

		found = append(found, domain)
	}

	for _, candidate := range append(
		ipv4Regex.FindAllString(text, -1),
		ipv6Regex.FindAllString(text, -1)...,
	) {
		ip := longestValidIP(candidate)
		if ip == "" || isPrivateIP(ip) || seen[ip] {
			continue
		}

		seen[ip] = true

		found = append(found, ip)
	}

	return found
}

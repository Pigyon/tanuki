package tanuki

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

type mappingEntry struct {
	Type, Real, Fiction string
}

type termEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type wildcardMapping struct {
	suffix  string
	fiction string
}

type rewriter struct {
	ac        *acMachine
	regex     []regexEntry
	wildcards []wildcardMapping
}

type regexEntry struct {
	re *regexp.Regexp
	to string
}

//go:embed terminology.json
var terminologyJSON []byte

//go:embed tlds.json
var tldsJSON []byte

var terminology []termEntry
var compiledTermRegexes []regexEntry
var commonTLDs map[string]bool

func init() {
	if err := json.Unmarshal(terminologyJSON, &terminology); err != nil {
		panic("invalid terminology.json: " + err.Error())
	}

	var tldsList []string
	if err := json.Unmarshal(tldsJSON, &tldsList); err != nil {
		panic("invalid tlds.json: " + err.Error())
	}

	commonTLDs = make(map[string]bool, len(tldsList))
	for _, tld := range tldsList {
		commonTLDs[tld] = true
	}

	sorted := make([]termEntry, len(terminology))
	copy(sorted, terminology)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].From) > len(sorted[j].From)
	})

	for _, t := range sorted {
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(t.From) + `\b`)
		if err == nil {
			compiledTermRegexes = append(compiledTermRegexes, regexEntry{re, t.To})
		}
	}
}

func loadMappings(engDir string) []mappingEntry {
	data, err := os.ReadFile(filepath.Join(engDir, "mappings.conf"))
	if err != nil {
		return nil
	}

	mappings := []mappingEntry{}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "|", 3)
		if len(parts) == 3 {
			mappings = append(mappings, mappingEntry{Type: parts[0], Real: parts[1], Fiction: parts[2]})
		}
	}

	return mappings
}

func newRewriter(mappings []mappingEntry, direction string) *rewriter {
	rw := &rewriter{}

	sorted := make([]mappingEntry, len(mappings))
	copy(sorted, mappings)

	sort.Slice(sorted, func(i, j int) bool {
		if direction == "r2f" {
			return len(sorted[i].Real) > len(sorted[j].Real)
		}

		return len(sorted[i].Fiction) > len(sorted[j].Fiction)
	})

	var patterns, replacements []string

	for _, m := range sorted {
		if m.Type == "wildcard" {
			if direction == "r2f" {
				suffix := strings.TrimPrefix(m.Real, "*")
				rw.wildcards = append(rw.wildcards, wildcardMapping{
					suffix:  suffix,
					fiction: m.Fiction,
				})
			}

			continue
		}

		if direction == "r2f" {
			patterns = append(patterns, m.Real)
			replacements = append(replacements, m.Fiction)
		} else {
			patterns = append(patterns, m.Fiction)
			replacements = append(replacements, m.Real)
		}
	}

	if len(patterns) > 0 {
		rw.ac = newACMachine(patterns, replacements)
	}

	if direction == "r2f" {
		rw.regex = compiledTermRegexes
	}

	return rw
}

func (rw *rewriter) rewrite(text string) string {
	if rw.ac != nil {
		text = rw.ac.replaceAll(text)
	}

	for _, wc := range rw.wildcards {
		text = rewriteWildcardR2F(text, wc)
	}

	for _, r := range rw.regex {
		text = r.re.ReplaceAllString(text, r.to)
	}

	return text
}

func rewriteWildcardR2F(text string, wc wildcardMapping) string {
	for _, match := range domainRegex.FindAllString(text, -1) {
		if strings.HasSuffix(match, wc.suffix) && match != strings.TrimPrefix(wc.suffix, ".") {
			text = strings.ReplaceAll(text, match, wc.fiction)
		}
	}

	return text
}

func mappingExists(engDir, typ, realVal string) bool {
	data, _ := os.ReadFile(filepath.Join(engDir, "mappings.conf"))
	return strings.Contains(string(data), typ+"|"+realVal+"|")
}

func addMapping(engDir, typ, realVal, fiction string) error {
	return appendToFile(
		filepath.Join(engDir, "mappings.conf"),
		fmt.Sprintf("%s|%s|%s", typ, realVal, fiction),
	)
}

func nextCounter(engDir, filename, defaultVal string) (int, error) {
	path := filepath.Join(engDir, filename)

	unlock := acquireFileLock(path)
	defer unlock()

	val := readFileContent(path)

	n, err := strconv.Atoi(val)
	if err != nil {
		n, _ = strconv.Atoi(defaultVal)
	}

	if err := writeFileContent(path, strconv.Itoa(n+1)); err != nil {
		return 0, err
	}

	return n, nil
}

func extractBaseDomain(domain string) string {
	base, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err == nil {
		return base
	}

	parts := strings.Split(domain, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "." + parts[len(parts)-1]
	}

	return domain
}

func extractOrgName(domain string) string {
	base := extractBaseDomain(domain)
	suffix, _ := publicsuffix.PublicSuffix(domain)

	if suffix != "" && strings.HasSuffix(base, "."+suffix) {
		return strings.TrimSuffix(base, "."+suffix)
	}

	parts := strings.Split(base, ".")
	if len(parts) >= 2 {
		return parts[0]
	}

	return domain
}

//nolint:funlen // addDomain builds many mapping lines for a single domain
func addDomain(engDir, domain, fictionOrg string) error {
	domain = strings.TrimRight(domain, ".")

	orgName := extractOrgName(domain)
	if orgName == "" {
		return fmt.Errorf("invalid domain: %s", domain)
	}

	if fictionOrg == "" {
		fictionOrg = readFileContent(filepath.Join(engDir, "fiction_org"))
		if fictionOrg == "" {
			fictionOrg = "DEVTARGET"
		}
	}

	baseDomain := extractBaseDomain(domain)
	orgLower := strings.ToLower(orgName)
	orgUpper := strings.ToUpper(orgName)
	orgCap := strings.ToUpper(orgLower[:1]) + orgLower[1:]
	fictionLower := strings.ToLower(fictionOrg)
	fictionUpper := strings.ToUpper(fictionOrg)

	var lines []string

	mappingsPath := filepath.Join(engDir, "mappings.conf")

	if !mappingExists(engDir, "domain", baseDomain) {
		basePort, err := nextCounter(engDir, "port_counter", portStart())
		if err != nil {
			return err
		}

		if basePort > 65535 {
			logger.Warn("port range exhausted", "domain", domain)
			return nil
		}

		ticketPrefix := orgUpper
		if len(ticketPrefix) > 4 {
			ticketPrefix = ticketPrefix[:4]
		}

		lines = append(lines,
			fmt.Sprintf("domain|%s|localhost:%d", baseDomain, basePort),
			fmt.Sprintf("org|%s|%s", orgCap, fictionOrg),
			fmt.Sprintf("org|%s|%s", orgLower, fictionLower),
			fmt.Sprintf("org|%s|%s", orgUpper, fictionUpper),
			fmt.Sprintf("email|@%s|@%s.local", baseDomain, fictionLower),
			fmt.Sprintf("path|/%s/|/dev/project/", orgLower),
			fmt.Sprintf("cloud|s3://%s|file:///tmp/%s", orgLower, fictionLower),
			fmt.Sprintf("ticket|%s-|DT-", ticketPrefix),
		)
	}

	if !mappingExists(engDir, "wildcard", "*."+baseDomain) {
		wildcardPort, err := nextCounter(engDir, "port_counter", portStart())
		if err != nil {
			return err
		}

		if wildcardPort > 65535 {
			logger.Warn("port range exhausted for wildcard", "domain", baseDomain)
		} else {
			lines = append(lines, fmt.Sprintf("wildcard|*.%s|localhost:%d", baseDomain, wildcardPort))
		}
	}

	if domain != baseDomain && !mappingExists(engDir, "domain", domain) {
		subPort, err := nextCounter(engDir, "port_counter", portStart())
		if err != nil {
			return err
		}

		if subPort > 65535 {
			logger.Warn("port range exhausted for subdomain", "domain", domain)
		} else {
			lines = append(lines, fmt.Sprintf("domain|%s|localhost:%d", domain, subPort))
		}
	}

	if len(lines) > 0 {
		return appendToFile(mappingsPath, strings.Join(lines, "\n"))
	}

	return nil
}

func addIPMapping(engDir, ip string) error {
	if isPrivateIP(ip) {
		return nil
	}

	if mappingExists(engDir, "ip", ip) {
		return nil
	}

	n, err := nextCounter(engDir, "ip_counter", "2")
	if err != nil {
		return err
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil
	}

	var fictionIP string

	if parsed.To4() != nil {
		if n/254 > 255 {
			logger.Warn("fiction IP range exhausted", "ip", ip)
			return nil
		}

		fictionIP = fmt.Sprintf("127.0.%d.%d", n/254, n%254+1)
	} else {
		fictionIP = fmt.Sprintf("::ffff:127.0.%d.%d", n/254, n%254+1)
	}

	return addMapping(engDir, "ip", ip, fictionIP)
}

func isPrivateIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true
	}

	return parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsUnspecified()
}

func showMappings() error {
	eng, err := requireEngagement()
	if err != nil {
		return err
	}

	mappings := loadMappings(engagementDir(eng))
	if len(mappings) == 0 {
		fmt.Println("No mappings found.")
		return nil
	}

	fmt.Printf("%-10s %-40s %-30s\n", "TYPE", "REAL", "FICTION")
	fmt.Printf("%-10s %-40s %-30s\n", "----", "----", "-------")

	for _, m := range mappings {
		fmt.Printf("%-10s %-40s %-30s\n", m.Type, m.Real, m.Fiction)
	}

	return nil
}

var ipv4Regex = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)
var ipv6Regex = regexp.MustCompile(
	`(?i)\b([0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b` +
		`|(?i)\b([0-9a-f]{1,4}:){1,6}:[0-9a-f]{1,4}\b` +
		`|(?i)\b::([0-9a-f]{1,4}:){0,5}[0-9a-f]{1,4}\b`,
)

func findNewPublicIPs(text, engDir string) []string {
	seen := make(map[string]bool)
	var result []string

	candidates := ipv4Regex.FindAllString(text, -1)
	candidates = append(candidates, ipv6Regex.FindAllString(text, -1)...)

	for _, ip := range candidates {
		if seen[ip] {
			continue
		}

		seen[ip] = true

		if net.ParseIP(ip) == nil || isPrivateIP(ip) || mappingExists(engDir, "ip", ip) {
			continue
		}

		result = append(result, ip)
	}

	return result
}

var domainRegex = regexp.MustCompile(`\b([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}\b`)

func hasCommonTLD(domain string) bool {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return false
	}

	return commonTLDs[strings.ToLower(parts[len(parts)-1])]
}

func findNewDomains(text, engDir string) []string {
	seen := make(map[string]bool)
	result := []string{}

	for _, domain := range domainRegex.FindAllString(text, -1) {
		if !hasCommonTLD(domain) || seen[domain] {
			continue
		}

		seen[domain] = true

		baseDomain := extractBaseDomain(domain)

		if !seen[baseDomain] {
			seen[baseDomain] = true

			if !mappingExists(engDir, "domain", baseDomain) {
				result = append(result, baseDomain)
			}
		}

		if domain != baseDomain && !mappingExists(engDir, "domain", domain) {
			result = append(result, domain)
		}
	}

	return result
}

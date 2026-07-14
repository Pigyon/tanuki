package main

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
)

type mappingEntry struct {
	Type, Real, Fiction string
}

type termEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type rewriter struct {
	replacer *strings.Replacer
	regex    []regexEntry
}

type regexEntry struct {
	re *regexp.Regexp
	to string
}

//go:embed terminology.json
var terminologyJSON []byte

var terminology []termEntry
var compiledTermRegexes []regexEntry

func init() {
	if err := json.Unmarshal(terminologyJSON, &terminology); err != nil {
		panic("invalid terminology.json: " + err.Error())
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

	var pairs []string

	for _, m := range mappings {
		if direction == "r2f" {
			pairs = append(pairs, m.Real, m.Fiction)
		} else {
			pairs = append(pairs, m.Fiction, m.Real)
		}
	}

	if len(pairs) > 0 {
		rw.replacer = strings.NewReplacer(pairs...)
	}

	if direction == "r2f" {
		rw.regex = compiledTermRegexes
	}

	return rw
}

func (rw *rewriter) rewrite(text string) string {
	if rw.replacer != nil {
		text = rw.replacer.Replace(text)
	}

	for _, r := range rw.regex {
		text = r.re.ReplaceAllString(text, r.to)
	}

	return text
}

// --- Mapping persistence ---

func mappingExists(engDir, typ, realVal string) bool {
	data, _ := os.ReadFile(filepath.Join(engDir, "mappings.conf"))
	return strings.Contains(string(data), typ+"|"+realVal+"|")
}

func addMapping(engDir, typ, realVal, fiction string) {
	appendToFile(filepath.Join(engDir, "mappings.conf"),
		fmt.Sprintf("%s|%s|%s", typ, realVal, fiction))
}

func nextCounter(engDir, filename, defaultVal string) int {
	val := readFileContent(filepath.Join(engDir, filename))

	n, err := strconv.Atoi(val)
	if err != nil {
		n, _ = strconv.Atoi(defaultVal)
	}

	writeFileContent(filepath.Join(engDir, filename), strconv.Itoa(n+1))

	return n
}

// --- Domain parsing ---

func extractBaseDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "." + parts[len(parts)-1]
	}

	return domain
}

func extractOrgName(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}

	return domain
}

// --- High-level mapping operations ---

func addDomain(domain, fictionOrg string) {
	domain = strings.TrimRight(domain, ".")

	eng := requireEngagement()
	engDir := engagementDir(eng)

	orgName := extractOrgName(domain)
	if orgName == "" {
		fatal("invalid domain: " + domain)
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

	if !mappingExists(engDir, "domain", baseDomain) {
		basePort := nextCounter(engDir, "port_counter", portStart())
		addMapping(engDir, "domain", baseDomain, fmt.Sprintf("localhost:%d", basePort))
		addMapping(engDir, "org", orgCap, fictionOrg)
		addMapping(engDir, "org", orgLower, fictionLower)
		addMapping(engDir, "org", orgUpper, fictionUpper)
		addMapping(engDir, "email", "@"+baseDomain, "@"+fictionLower+".local")
		addMapping(engDir, "path", "/"+orgLower+"/", "/dev/project/")
		addMapping(engDir, "cloud", "s3://"+orgLower, "file:///tmp/"+fictionLower)

		ticketPrefix := orgUpper
		if len(ticketPrefix) > 4 {
			ticketPrefix = ticketPrefix[:4]
		}

		addMapping(engDir, "ticket", ticketPrefix+"-", "DT-")
	}

	if domain != baseDomain && !mappingExists(engDir, "domain", domain) {
		subPort := nextCounter(engDir, "port_counter", portStart())
		addMapping(engDir, "domain", domain, fmt.Sprintf("localhost:%d", subPort))
	}
}

func addIPMapping(ip string) {
	if isPrivateIP(ip) {
		return
	}

	eng := requireEngagement()

	engDir := engagementDir(eng)
	if mappingExists(engDir, "ip", ip) {
		return
	}

	n := nextCounter(engDir, "ip_counter", "2")
	fictionIP := fmt.Sprintf("127.0.%d.%d", n/254, n%254+1)
	addMapping(engDir, "ip", ip, fictionIP)
}

func isPrivateIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true
	}

	return parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsUnspecified()
}

func showMappings() {
	eng := requireEngagement()

	mappings := loadMappings(engagementDir(eng))
	if len(mappings) == 0 {
		fmt.Println("No mappings found.")
		return
	}

	fmt.Printf("%-10s %-40s %-30s\n", "TYPE", "REAL", "FICTION")
	fmt.Printf("%-10s %-40s %-30s\n", "----", "----", "-------")

	for _, m := range mappings {
		fmt.Printf("%-10s %-40s %-30s\n", m.Type, m.Real, m.Fiction)
	}
}

// --- Auto-detection in text ---

var ipRegex = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)

func findNewPublicIPs(text string) []string {
	eng := getEngagement()
	if eng == "" {
		return nil
	}

	engDir := engagementDir(eng)
	seen := make(map[string]bool)
	result := []string{}

	for _, ip := range ipRegex.FindAllString(text, -1) {
		if seen[ip] || net.ParseIP(ip) == nil || isPrivateIP(ip) || mappingExists(engDir, "ip", ip) {
			continue
		}

		seen[ip] = true

		result = append(result, ip)
	}

	return result
}

var domainRegex = regexp.MustCompile(`\b([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}\b`)

var commonTLDs = map[string]bool{
	"com": true, "org": true, "net": true, "io": true, "co": true,
	"gov": true, "edu": true, "mil": true,
	"info": true, "biz": true, "dev": true, "app": true,
	"cloud": true, "tech": true, "online": true, "site": true,
	"xyz": true, "me": true, "tv": true, "cc": true,
	"uk": true, "de": true, "fr": true, "jp": true, "cn": true,
	"au": true, "ca": true, "br": true, "in": true, "ru": true,
	"nl": true, "it": true, "es": true, "se": true, "no": true,
	"fi": true, "dk": true, "pl": true, "cz": true, "at": true,
	"ch": true, "be": true, "ie": true, "il": true, "za": true,
	"kr": true, "sg": true, "hk": true, "tw": true, "nz": true,
	"mx": true, "ar": true, "cl": true, "pt": true, "hu": true,
}

func hasCommonTLD(domain string) bool {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return false
	}

	return commonTLDs[strings.ToLower(parts[len(parts)-1])]
}

func findNewDomains(text string) []string {
	eng := getEngagement()
	if eng == "" {
		return nil
	}

	engDir := engagementDir(eng)
	seen := make(map[string]bool)
	result := []string{}

	for _, domain := range domainRegex.FindAllString(text, -1) {
		if !hasCommonTLD(domain) || seen[domain] {
			continue
		}

		seen[domain] = true

		baseDomain := extractBaseDomain(domain)

		if !mappingExists(engDir, "domain", baseDomain) {
			result = append(result, baseDomain)
		}

		if domain != baseDomain && !mappingExists(engDir, "domain", domain) {
			result = append(result, domain)
		}
	}

	return result
}

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

// Rewrite directions. r2f replaces real values with fiction on the way to
// the API; f2r reverses it on the way back.
const (
	directionR2F = "r2f"
	directionF2R = "f2r"
)

// maxPort is the highest port a fiction localhost mapping can use. Running
// past it means a real value would have no fiction counterpart.
const maxPort = 65535

// maxFictionIPIndex is the highest ip_counter value that still renders inside
// 127.0.0.0/16: the last address the scheme can produce is 127.0.255.254.
const maxFictionIPIndex = 256*254 - 1

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
	orgRegex  []regexEntry
	wildcards []wildcardMapping
	// ipFictions maps a canonical real IP to its fiction, so every textual
	// form of that address (zero-padded, IPv6-compressed) is rewritten.
	ipFictions map[string]string
	// explicit holds every real value that has its own mapping, so the
	// wildcard pass leaves them for the exact-match pass to handle.
	explicit map[string]bool
}

type regexEntry struct {
	re *regexp.Regexp
	to string
}

//go:embed terminology.json
var terminologyJSON []byte

//go:embed tlds.json
var tldsJSON []byte

var (
	terminology         []termEntry
	compiledTermRegexes []regexEntry
	commonTLDs          map[string]bool
)

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
			compiledTermRegexes = append(compiledTermRegexes, regexEntry{re: re, to: t.To})
		}
	}
}

// loadMappings treats an unreadable file as empty; the proxy path uses
// readMappingsFile instead so a read error can fail closed.
func loadMappings(engDir string) []mappingEntry {
	mappings, err := readMappingsFile(engDir)
	if err != nil {
		return nil
	}

	return mappings
}

// readMappingsFile distinguishes a read error from a file with zero mappings.
func readMappingsFile(engDir string) ([]mappingEntry, error) {
	data, err := os.ReadFile(filepath.Join(engDir, "mappings.conf"))
	if err != nil {
		return nil, err
	}

	return parseMappings(string(data), engDir), nil
}

func parseMappings(data, engDir string) []mappingEntry {
	mappings := []mappingEntry{}
	malformed := 0

	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "|", 3)
		if len(parts) == 3 {
			mappings = append(mappings, mappingEntry{Type: parts[0], Real: parts[1], Fiction: parts[2]})
			continue
		}

		// An unparsed line is a value that won't be rewritten; surface it.
		malformed++
	}

	if malformed > 0 && logger != nil {
		logger.Warn("skipped malformed mapping lines", "count", malformed, "engagement_dir", engDir)
	}

	return mappings
}

func newRewriter(mappings []mappingEntry, direction string) *rewriter {
	rw := &rewriter{
		explicit:   make(map[string]bool, len(mappings)),
		ipFictions: map[string]string{},
	}

	sorted := make([]mappingEntry, len(mappings))
	copy(sorted, mappings)

	sort.Slice(sorted, func(i, j int) bool {
		if direction == directionR2F {
			return len(sorted[i].Real) > len(sorted[j].Real)
		}

		return len(sorted[i].Fiction) > len(sorted[j].Fiction)
	})

	var patterns, replacements []string

	for _, m := range sorted {
		switch {
		case m.Type == "wildcard":
			if direction == directionR2F {
				suffix := strings.ToLower(strings.TrimPrefix(m.Real, "*"))
				rw.wildcards = append(rw.wildcards, wildcardMapping{suffix: suffix, fiction: m.Fiction})
			}

		case m.Type == "ip" && direction == directionR2F:
			// Matched by canonical value so every textual form is caught.
			if c := canonicalIP(m.Real); c != "" {
				rw.ipFictions[c] = m.Fiction
			}

		case m.Type == "org":
			rw.addOrgRule(m, direction)

		case direction == directionR2F:
			patterns = append(patterns, m.Real)
			replacements = append(replacements, m.Fiction)
			rw.explicit[strings.ToLower(m.Real)] = true

		default:
			patterns = append(patterns, m.Fiction)
			replacements = append(replacements, m.Real)
		}
	}

	if len(patterns) > 0 {
		rw.ac = newACMachine(patterns, replacements)
	}

	if direction == directionR2F {
		rw.regex = compiledTermRegexes
	}

	return rw
}

// addOrgRule compiles an org mapping into a word-bounded regex so a short org
// name ("ge", "hp") does not shred unrelated words ("message" -> "messaDEV...").
func (rw *rewriter) addOrgRule(m mappingEntry, direction string) {
	from, to := m.Real, m.Fiction
	if direction != directionR2F {
		from, to = m.Fiction, m.Real
	}

	if re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(from) + `\b`); err == nil {
		rw.orgRegex = append(rw.orgRegex, regexEntry{re: re, to: to})
	}
}

func (rw *rewriter) rewrite(text string) string {
	// Wildcards first, or the exact-match pass rewrites the base domain inside
	// an unmapped subdomain and leaks the label upstream.
	for _, wc := range rw.wildcards {
		text = rw.rewriteWildcard(text, wc)
	}

	text = rw.rewriteIPs(text)

	if rw.ac != nil {
		text = rw.ac.replaceAll(text)
	}

	// Org after the automaton, so a mapped domain is consumed before its bare
	// org name is matched.
	for _, r := range rw.orgRegex {
		text = r.re.ReplaceAllString(text, r.to)
	}

	for _, r := range rw.regex {
		text = r.re.ReplaceAllString(text, r.to)
	}

	return text
}

// rewriteWildcard replaces wc-covered subdomains lacking their own mapping;
// explicit hosts are skipped so the exact pass gives them a reversible value.
func (rw *rewriter) rewriteWildcard(text string, wc wildcardMapping) string {
	base := strings.TrimPrefix(wc.suffix, ".")

	for _, match := range domainRegex.FindAllString(text, -1) {
		// Compare lowercased so "API.AMAZON.COM" is caught before the exact
		// pass rewrites its base and leaks the "API" label.
		lower := strings.ToLower(match)
		if lower == base || rw.explicit[lower] {
			continue
		}

		if strings.HasSuffix(lower, wc.suffix) {
			text = strings.ReplaceAll(text, match, wc.fiction)
		}
	}

	return text
}

// mappingKeys returns the "type|real" key of every mapping in engDir. Callers
// testing many candidates use it to read mappings.conf once instead of once
// per candidate.
func mappingKeys(engDir string) map[string]bool {
	keys := map[string]bool{}

	data, err := os.ReadFile(filepath.Join(engDir, "mappings.conf"))
	if err != nil {
		return keys
	}

	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if parts := strings.SplitN(line, "|", 3); len(parts) == 3 {
			keys[parts[0]+"|"+parts[1]] = true
		}
	}

	return keys
}

// mappingExists compares whole fields. A plain substring search also matched
// mapping *content*, so a custom rule whose fiction text happened to contain
// "domain|evil.com|" made that domain look mapped and it never got one.
func mappingExists(engDir, typ, realVal string) bool {
	return mappingKeys(engDir)[typ+"|"+realVal]
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

	n, err := readCounter(path, defaultVal)
	if err != nil {
		return 0, err
	}

	if err := writeFileContent(path, strconv.Itoa(n+1)); err != nil {
		return 0, err
	}

	return n, nil
}

// readCounter returns the next value to allocate from path. Only an absent or
// empty file falls back to defaultVal: a file that exists but cannot be read
// or parsed is an error, because restarting from the default would hand two
// real targets the same fiction value and make the reverse pass ambiguous.
func readCounter(path, defaultVal string) (int, error) {
	name := filepath.Base(path)

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("counter %s is unreadable: %w", name, err)
	}

	raw := strings.TrimSpace(string(data))
	if raw == "" {
		n, err := strconv.Atoi(defaultVal)
		if err != nil {
			return 0, fmt.Errorf("counter %s: invalid default %q: %w", name, defaultVal, err)
		}

		return n, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("counter %s is corrupt (%q): refusing to reallocate fiction values", name, raw)
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

// baseMappingLines builds the domain mapping and the org, email, path, cloud,
// and ticket values derived from it. Every fiction is tied to fictionOrg/orgNum
// so distinct real orgs reverse to distinct targets instead of colliding.
func baseMappingLines(baseDomain string, port, orgNum int, orgName, fictionOrg string) []string {
	org := strings.ToLower(orgName)
	fictionLower := strings.ToLower(fictionOrg)

	ticketReal := strings.ToUpper(org)
	if len(ticketReal) > 4 {
		ticketReal = ticketReal[:4]
	}

	ticketFiction := "DT"
	if orgNum > 1 {
		ticketFiction += strconv.Itoa(orgNum)
	}

	return []string{
		fmt.Sprintf("domain|%s|localhost:%d", baseDomain, port),
		fmt.Sprintf("org|%s|%s", org, fictionOrg),
		fmt.Sprintf("email|@%s|@%s.local", baseDomain, fictionLower),
		fmt.Sprintf("path|/%s/|/dev/%s/", org, fictionLower),
		fmt.Sprintf("cloud|s3://%s|file:///tmp/%s", org, fictionLower),
		fmt.Sprintf("ticket|%s-|%s-", ticketReal, ticketFiction),
	}
}

// allocateOrgFiction returns the next distinct fiction org: the base name for
// the first org, then base+2, base+3, … so no two real orgs share a fiction.
func allocateOrgFiction(engDir, base string) (num int, fiction string, err error) {
	n, err := nextCounter(engDir, "org_counter", "1")
	if err != nil {
		return 0, "", err
	}

	if n < 2 {
		return n, base, nil
	}

	return n, base + strconv.Itoa(n), nil
}

// orgNumberFromFiction recovers the allocation number from a fiction org (its
// trailing digits, or 1 when there are none), for resetting the counter.
func orgNumberFromFiction(fiction string) int {
	i := len(fiction)
	for i > 0 && fiction[i-1] >= '0' && fiction[i-1] <= '9' {
		i--
	}

	if i == len(fiction) {
		return 1
	}

	if n, err := strconv.Atoi(fiction[i:]); err == nil && n >= 1 {
		return n
	}

	return 1
}

// allocatePort takes the next fiction port, refusing to continue once the
// range is exhausted so no real value is left without a mapping.
func allocatePort(engDir, what string) (int, error) {
	port, err := nextCounter(engDir, "port_counter", portStart())
	if err != nil {
		return 0, err
	}

	if port > maxPort {
		return 0, fmt.Errorf("port range exhausted for %s (would leak to upstream)", what)
	}

	return port, nil
}

// portFromFiction extracts the port from a "localhost:<port>" domain fiction.
func portFromFiction(fiction string) (int, bool) {
	const prefix = "localhost:"

	if !strings.HasPrefix(fiction, prefix) {
		return 0, false
	}

	p, err := strconv.Atoi(fiction[len(prefix):])
	if err != nil || p <= 0 || p > maxPort {
		return 0, false
	}

	return p, true
}

// ipCounterFromFiction reverses the "127.0.a.b" (or IPv4-mapped IPv6) fiction
// back into the counter value that produced it, mirroring addIPMapping.
func ipCounterFromFiction(fiction string) (int, bool) {
	s := strings.TrimPrefix(fiction, "::ffff:")

	const prefix = "127.0."
	if !strings.HasPrefix(s, prefix) {
		return 0, false
	}

	parts := strings.Split(s[len(prefix):], ".")
	if len(parts) != 2 {
		return 0, false
	}

	high, err1 := strconv.Atoi(parts[0])
	low, err2 := strconv.Atoi(parts[1])

	// low is 1..254 and high 0..255: the exact range fictionIPFor emits.
	if err1 != nil || err2 != nil || high < 0 || high > 255 || low < 1 || low > 254 {
		return 0, false
	}

	return high*254 + (low - 1), true
}

// resetCountersFromMappings advances the counters past the values the current
// mappings use, so a post-import allocation cannot collide with an imported one.
func resetCountersFromMappings(engDir string) error {
	portNext := 9000
	if p, err := strconv.Atoi(portStart()); err == nil {
		portNext = p
	}

	ipNext := 2
	orgNext := 1

	for _, m := range loadMappings(engDir) {
		if p, ok := portFromFiction(m.Fiction); ok && p >= portNext {
			portNext = p + 1
		}

		if n, ok := ipCounterFromFiction(m.Fiction); ok && n >= ipNext {
			ipNext = n + 1
		}

		if m.Type == "org" {
			if n := orgNumberFromFiction(m.Fiction); n >= orgNext {
				orgNext = n + 1
			}
		}
	}

	if err := writeFileContent(filepath.Join(engDir, "port_counter"), strconv.Itoa(portNext)); err != nil {
		return err
	}

	if err := writeFileContent(filepath.Join(engDir, "ip_counter"), strconv.Itoa(ipNext)); err != nil {
		return err
	}

	return writeFileContent(filepath.Join(engDir, "org_counter"), strconv.Itoa(orgNext))
}

func resolveFictionOrg(engDir, fictionOrg string) string {
	if fictionOrg != "" {
		return fictionOrg
	}

	if stored := readFileContent(filepath.Join(engDir, "fiction_org")); stored != "" {
		return stored
	}

	return "DEVTARGET"
}

func addDomain(engDir, domain, fictionOrg string) error {
	// Lowercase so the table has no "Amazon.com"/"amazon.com" duplicates.
	domain = strings.ToLower(strings.TrimRight(domain, "."))

	orgName := extractOrgName(domain)
	if orgName == "" {
		return fmt.Errorf("invalid domain: %s", domain)
	}

	baseFiction := resolveFictionOrg(engDir, fictionOrg)
	baseDomain := extractBaseDomain(domain)
	mappingsPath := filepath.Join(engDir, "mappings.conf")

	// One snapshot for all three checks: nothing is appended until the end.
	existing := mappingKeys(engDir)

	var lines []string

	if !existing["domain|"+baseDomain] {
		port, err := allocatePort(engDir, "domain "+baseDomain)
		if err != nil {
			return err
		}

		orgNum, fiction, err := allocateOrgFiction(engDir, baseFiction)
		if err != nil {
			return err
		}

		lines = append(lines, baseMappingLines(baseDomain, port, orgNum, orgName, fiction)...)
	}

	if !existing["wildcard|*."+baseDomain] {
		port, err := allocatePort(engDir, "wildcard *."+baseDomain)
		if err != nil {
			return err
		}

		lines = append(lines, fmt.Sprintf("wildcard|*.%s|localhost:%d", baseDomain, port))
	}

	if domain != baseDomain && !existing["domain|"+domain] {
		port, err := allocatePort(engDir, "subdomain "+domain)
		if err != nil {
			return err
		}

		lines = append(lines, fmt.Sprintf("domain|%s|localhost:%d", domain, port))
	}

	if len(lines) > 0 {
		return appendToFile(mappingsPath, strings.Join(lines, "\n"))
	}

	return nil
}

func addIPMapping(engDir, ip string) error {
	canonical := canonicalIP(ip)
	if canonical == "" {
		// Not an address, so there is nothing to map and nothing to leak.
		return nil
	}

	ip = canonical

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

	fictionIP, err := fictionIPFor(ip, n)
	if err != nil {
		return err
	}

	return addMapping(engDir, "ip", ip, fictionIP)
}

// fictionIPFor renders counter n as a loopback fiction matching the address
// family of the real IP. Like allocatePort it refuses to continue once the
// range is exhausted: an IP with no fiction counterpart reaches the model
// verbatim. The guard covers both families, so IPv6 can no longer produce an
// unparseable address such as "::ffff:127.0.275.151".
func fictionIPFor(ip string, n int) (string, error) {
	if n > maxFictionIPIndex {
		return "", fmt.Errorf("fiction IP range exhausted for %s (would leak to upstream)", ip)
	}

	fiction := fmt.Sprintf("127.0.%d.%d", n/254, n%254+1)

	if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
		return "::ffff:" + fiction, nil
	}

	return fiction, nil
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

var (
	ipv4Regex = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)
	// Deliberately greedy: longestValidIP validates each candidate, and
	// under-matching would leave part of a real address unmapped (a leak).
	ipv6Regex = regexp.MustCompile(`(?i)[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,}(?:\.\d{1,3}){0,3}`)
)

// canonicalIP returns the canonical form of an IP, or "" if s is not one. It
// also accepts zero-padded IPv4 octets, which net.ParseIP rejects.
func canonicalIP(s string) string {
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}

	if stripped := stripLeadingZerosV4(s); stripped != s {
		if ip := net.ParseIP(stripped); ip != nil {
			return ip.String()
		}
	}

	return ""
}

func stripLeadingZerosV4(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return s
	}

	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return s
		}

		parts[i] = strconv.Itoa(n)
	}

	return strings.Join(parts, ".")
}

// longestValidIP returns the canonical form of the longest IP prefix of
// candidate, or "" if none parses (trimming trailing junk from a greedy match).
func longestValidIP(candidate string) string {
	ip, _ := longestValidIPPrefix(candidate)
	return ip
}

func longestValidIPPrefix(candidate string) (canonical string, prefixLen int) {
	for n := len(candidate); n > 0; n-- {
		if ip := canonicalIP(candidate[:n]); ip != "" {
			return ip, n
		}
	}

	return "", 0
}

// rewriteIPs replaces mapped IPs by canonical value so all textual forms
// collapse. IPv6 runs first because a match may embed an IPv4-mapped form.
func (rw *rewriter) rewriteIPs(text string) string {
	if len(rw.ipFictions) == 0 {
		return text
	}

	text = rw.replaceIPs(text, ipv6Regex)

	return rw.replaceIPs(text, ipv4Regex)
}

func (rw *rewriter) replaceIPs(text string, re *regexp.Regexp) string {
	locs := re.FindAllStringIndex(text, -1)
	if locs == nil {
		return text
	}

	var b strings.Builder

	pos := 0

	for _, loc := range locs {
		ip, n := longestValidIPPrefix(text[loc[0]:loc[1]])

		fiction, ok := rw.ipFictions[ip]
		if n == 0 || !ok {
			continue
		}

		b.WriteString(text[pos:loc[0]])
		b.WriteString(fiction)

		pos = loc[0] + n
	}

	if pos == 0 {
		return text
	}

	b.WriteString(text[pos:])

	return b.String()
}

func findNewPublicIPs(text, engDir string) []string {
	seen := make(map[string]bool)
	existing := mappingKeys(engDir)
	result := []string{}

	candidates := ipv4Regex.FindAllString(text, -1)
	candidates = append(candidates, ipv6Regex.FindAllString(text, -1)...)

	for _, candidate := range candidates {
		ip := longestValidIP(candidate)
		if ip == "" || seen[ip] {
			continue
		}

		seen[ip] = true

		if isPrivateIP(ip) || existing["ip|"+ip] {
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
	seen := make(map[string]bool)    // candidates already examined
	emitted := make(map[string]bool) // values already in result
	existing := mappingKeys(engDir)  // mappings already on disk
	result := []string{}

	for _, raw := range domainRegex.FindAllString(text, -1) {
		domain := strings.ToLower(raw)
		if !hasCommonTLD(domain) || seen[domain] {
			continue
		}

		seen[domain] = true

		// Base domain first so it gets the lower port and owns the derived
		// org/email/path mappings; emitted dedupes the apex-only case.
		for _, candidate := range []string{extractBaseDomain(domain), domain} {
			if emitted[candidate] || existing["domain|"+candidate] {
				continue
			}

			emitted[candidate] = true

			result = append(result, candidate)
		}
	}

	return result
}

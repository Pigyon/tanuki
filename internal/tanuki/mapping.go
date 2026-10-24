package tanuki

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// Mapping types, as written in the first field of a mappings.conf line.
const (
	mapDomain   = "domain"
	mapWildcard = "wildcard"
	mapOrg      = "org"
	mapEmail    = "email"
	mapPath     = "path"
	mapCloud    = "cloud"
	mapIP       = "ip"
	mapAccount  = "account"
	mapCustom   = "custom"
)

// defaultFictionOrg is the fiction org name when none is configured.
const defaultFictionOrg = "DEVTARGET"

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
	base    string // suffix without the leading dot: the apex itself
	fiction string
}

type rewriter struct {
	ac        *acMachine
	terms     *alternationRewriter
	orgs      *acMachine
	wildcards []wildcardMapping
	// ipFictions maps a canonical real IP to its fiction, so every textual
	// form of that address (zero-padded, IPv6-compressed) is rewritten.
	ipFictions map[string]string
	// explicit holds every real value that has its own mapping, so the
	// wildcard pass leaves them for the exact-match pass to handle.
	explicit map[string]bool
	// heads holds every prefix of every fiction value, lowercased (f2r only),
	// so a stream can tell whether its tail may be the start of one.
	heads   map[string]bool
	maxHead int
	// residue finds the org names and custom-rule values a rewrite should have
	// removed (r2f only), for the fail-closed gate; see survivors.
	residue *acMachine
	// memo remembers rewrites of long strings. A rewriter is rebuilt whenever
	// the mappings change, so a remembered result is never stale.
	memo memoCache[string]
}

//go:embed terminology.json
var terminologyJSON []byte

//go:embed tlds.json
var tldsJSON []byte

var (
	terminology  []termEntry
	termRewriter *alternationRewriter
	commonTLDs   map[string]bool
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

	termRewriter = newAlternationRewriter(terminology)
}

// alternationRewriter applies a set of word-bounded literal rules in a single
// pass. Running each rule as its own ReplaceAll dominated the cost of a
// rewrite: the 29 terminology rules alone meant 29 full scans of every
// request body and of every SSE event.
type alternationRewriter struct {
	re *regexp.Regexp
	to map[string]string // lowercased match -> replacement
}

// newAlternationRewriter returns nil when there is nothing to rewrite, which
// the rewrite method treats as a no-op.
func newAlternationRewriter(rules []termEntry) *alternationRewriter {
	sorted := make([]termEntry, len(rules))
	copy(sorted, rules)

	// Longest first: the alternation is leftmost-first, so a longer phrase
	// has to be offered before any rule that is a prefix of it.
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].From) > len(sorted[j].From)
	})

	to := make(map[string]string, len(sorted))
	alts := make([]string, 0, len(sorted))

	for _, r := range sorted {
		key := strings.ToLower(r.From)
		if key == "" {
			continue
		}

		// First rule wins, matching what sequential passes did.
		if _, dup := to[key]; dup {
			continue
		}

		to[key] = r.To
		alts = append(alts, regexp.QuoteMeta(r.From))
	}

	if len(alts) == 0 {
		return nil
	}

	// QuoteMeta escapes every rule, so the pattern is always valid: compiling
	// it must not fail quietly, or the rules would silently stop applying.
	return &alternationRewriter{
		re: regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`),
		to: to,
	}
}

func (a *alternationRewriter) rewrite(text string) string {
	if a == nil {
		return text
	}

	return a.re.ReplaceAllStringFunc(text, func(match string) string {
		if to, ok := a.to[strings.ToLower(match)]; ok {
			return to
		}

		return match
	})
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
			// Older engagements derived a "ticket" prefix per org. It matched
			// case-insensitively inside any hyphenated word and reversed to
			// upper case ("acme-api" came back as "ACME-api"), so it is skipped.
			if parts[0] != "ticket" {
				mappings = append(mappings, mappingEntry{Type: parts[0], Real: parts[1], Fiction: parts[2]})
			}

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
	return newRewriterEscaping(mappings, direction, nil)
}

func noEscape(s string) string { return s }

// newRewriterEscaping builds a rewriter whose replacements go through escape,
// for text that is itself inside a JSON string: a real value carrying a quote
// or a backslash must be escaped as it is inserted, or the fragment it lands
// in stops being JSON.
func newRewriterEscaping(mappings []mappingEntry, direction string, escape func(string) string) *rewriter {
	if escape == nil {
		escape = noEscape
	}

	rw := &rewriter{
		explicit:   make(map[string]bool, len(mappings)),
		ipFictions: map[string]string{},
	}

	sorted := make([]mappingEntry, len(mappings))
	copy(sorted, mappings)

	// Stable, so which of two equal-length rules for one value comes first,
	// and therefore wins, is the table's order rather than chance.
	sort.SliceStable(sorted, func(i, j int) bool {
		if direction == directionR2F {
			return len(sorted[i].Real) > len(sorted[j].Real)
		}

		return len(sorted[i].Fiction) > len(sorted[j].Fiction)
	})

	var (
		patterns     []string
		replacements []string
		bounds       []boundKind
		orgRules     []termEntry
	)

	for _, m := range sorted {
		switch {
		case m.Type == mapWildcard:
			if direction == directionR2F {
				suffix := strings.ToLower(strings.TrimPrefix(m.Real, "*"))
				rw.wildcards = append(rw.wildcards, wildcardMapping{
					suffix:  suffix,
					base:    strings.TrimPrefix(suffix, "."),
					fiction: m.Fiction,
				})
			}

		case m.Type == mapIP && direction == directionR2F:
			// Matched by canonical value so every textual form is caught.
			if c := canonicalIP(m.Real); c != "" {
				rw.ipFictions[c] = m.Fiction
			}

		case m.Type == mapOrg:
			orgRules = append(orgRules, orgRule(m, direction, escape))

		case direction == directionR2F:
			patterns = append(patterns, m.Real)
			replacements = append(replacements, m.Fiction)
			bounds = append(bounds, mappingBound(m.Type))
			rw.explicit[strings.ToLower(m.Real)] = true

		default:
			patterns = append(patterns, m.Fiction)
			replacements = append(replacements, escape(m.Real))
			bounds = append(bounds, mappingBound(m.Type))
		}
	}

	if len(patterns) > 0 {
		rw.ac = newACMachine(patterns, replacements, bounds)
	}

	rw.orgs = newOrgMachine(orgRules)

	if direction == directionR2F {
		rw.terms = termRewriter
		rw.residue = newResidueMachine(mappings)
	} else {
		rw.heads, rw.maxHead = fictionHeads(mappings)
	}

	return rw
}

// fictionHeads returns every prefix of every reversible fiction value,
// lowercased, and the longest value's length. Wildcard fictions are left out:
// they are never reversed.
func fictionHeads(mappings []mappingEntry) (map[string]bool, int) {
	heads := map[string]bool{}
	longest := 0

	for _, m := range mappings {
		if m.Type == mapWildcard {
			continue
		}

		fiction := strings.ToLower(m.Fiction)
		for i := 1; i <= len(fiction); i++ {
			heads[fiction[:i]] = true
		}

		longest = max(longest, len(fiction))
	}

	return heads, longest
}

// splitHeld splits streamed text into what can be sent now and the tail to
// hold back: the longest suffix that is the start of a fiction value, or a
// whole one, since the next delta may complete it or run on into a different
// name.
func (rw *rewriter) splitHeld(text string) (send, hold string) {
	for k := min(len(text), rw.maxHead); k > 0; k-- {
		if rw.heads[strings.ToLower(text[len(text)-k:])] {
			return text[:len(text)-k], text[len(text)-k:]
		}
	}

	return text, ""
}

// mappingBound says where a mapping's value may match. Hostnames must not
// match inside a longer name. Types whose values are deliberately open ended
// are not bounded: "s3://acme" is meant to cover every bucket that starts with
// it, so there matching a longer string is the point. Custom rules keep the
// same greedy behaviour.
func mappingBound(typ string) boundKind {
	switch typ {
	case mapDomain, mapEmail:
		return boundHost
	case mapAccount:
		// Not inside a longer run of digits or an identifier.
		return boundWord
	case mapIP:
		// Reversing an address glued to more digits built a different one:
		// with 127.0.0.3 mapped, "127.0.0.35" became "52.94.236.2485", and
		// where that lands on a valid address a tool goes to a host that is
		// not in scope. Only the f2r direction reaches here; r2f matches an
		// address through findIPs, which already requires it to stand alone.
		return boundAddress
	}

	return boundNone
}

// minOrgPartLen is the shortest org name matched as part of an identifier.
// A shorter one is too common a fragment: "x" would fire in "x_axis" and "ge"
// in "isGe", so those still have to stand as a whole word.
const minOrgPartLen = 4

// orgRule orients an org mapping for the given direction.
func orgRule(m mappingEntry, direction string, escape func(string) string) termEntry {
	if direction == directionR2F {
		return termEntry{From: m.Real, To: m.Fiction}
	}

	return termEntry{From: m.Fiction, To: escape(m.Real)}
}

// newOrgMachine matches org names as words, or as parts of an identifier
// ("acme_prod", "AcmeCloud"), never inside an unrelated word, so a short one
// ("ge") does not shred "message" into "messaDEV...". Returns nil when there
// are no rules.
func newOrgMachine(rules []termEntry) *acMachine {
	var (
		patterns, replacements []string
		bounds                 []boundKind
	)

	seen := map[string]bool{}

	for _, r := range rules {
		key := strings.ToLower(r.From)
		if key == "" || seen[key] {
			continue
		}

		// Rules arrive longest first, so the first for a name wins.
		seen[key] = true

		bound := boundWord
		if len(r.From) >= minOrgPartLen {
			bound = boundWordPart
		}

		patterns = append(patterns, r.From)
		replacements = append(replacements, r.To)
		bounds = append(bounds, bound)
	}

	if len(patterns) == 0 {
		return nil
	}

	return newACMachine(patterns, replacements, bounds)
}

func (rw *rewriter) rewrite(text string) string {
	return rw.memo.get(text, rw.rewriteText)
}

func (rw *rewriter) rewriteText(text string) string {
	// Wildcards first, or the exact-match pass rewrites the base domain inside
	// an unmapped subdomain and leaks the label upstream.
	text = rw.rewriteWildcards(text)

	text = rw.rewriteIPs(text)

	if rw.ac != nil {
		text = rw.ac.replaceAll(text)
	}

	// Terminology before org, so an org that happens to share a word with a
	// replacement term is still rewritten and the gate never sees it.
	text = rw.terms.rewrite(text)

	// Org after the automaton, so a mapped domain is consumed before its bare
	// org name is matched.
	if rw.orgs != nil {
		text = rw.orgs.replaceAll(text)
	}

	return text
}

// newResidueMachine matches every org name and custom-rule value with the
// bounds the rewrite uses. Domains, emails and IPs are left to the target
// scan, which also catches the ones that were never mapped.
func newResidueMachine(mappings []mappingEntry) *acMachine {
	var orgRules []termEntry

	var custom []string

	for _, m := range mappings {
		switch m.Type {
		case mapOrg:
			orgRules = append(orgRules, orgRule(m, directionR2F, noEscape))
		case mapCustom:
			custom = append(custom, m.Real)
		}
	}

	orgs := newOrgMachine(orgRules)
	if orgs == nil && len(custom) == 0 {
		return nil
	}

	patterns := slices.Clone(custom)
	bounds := make([]boundKind, len(custom))

	if orgs != nil {
		patterns = append(patterns, orgs.patterns...)
		bounds = append(bounds, orgs.bounds...)
	}

	return newACMachine(patterns, patterns, bounds)
}

// survivors returns the mapped org names and custom-rule values still in text
// that has been rewritten. Each is matched exactly as the rewrite matched it,
// so anything found is a value the rewrite failed to replace.
func (rw *rewriter) survivors(text string) []string {
	if rw.residue == nil {
		return nil
	}

	var found []string

	for _, m := range rw.residue.find(text) {
		found = append(found, strings.ToLower(rw.residue.patterns[m.index]))
	}

	slices.Sort(found)

	return slices.Compact(found)
}

// rewriteWildcards replaces every wildcard-covered hostname that has no
// mapping of its own; explicit hosts are left for the exact pass, which gives
// them a reversible value.
//
// All wildcards share one scan. Scanning the whole text once per wildcard and
// then calling strings.ReplaceAll for each match was the most expensive part
// of a rewrite, and quadratic in the number of times a hostname appeared.
func (rw *rewriter) rewriteWildcards(text string) string {
	if len(rw.wildcards) == 0 {
		return text
	}

	locs := domainRegex.FindAllStringIndex(text, -1)
	if locs == nil {
		return text
	}

	var b strings.Builder

	pos := 0

	for _, loc := range locs {
		// Compare lowercased so "API.AMAZON.COM" is caught before the exact
		// pass rewrites its base and leaks the "API" label.
		host := strings.ToLower(text[loc[0]:loc[1]])

		fiction, n, ok := rw.wildcardPrefix(host)
		if !ok {
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

// wildcardPrefix returns the fiction covering host and how much of host it
// covers. host must already be lowercased.
//
// Each whole-label prefix is tried, longest first, so a target carrying a file
// extension ("bounty.acme.com.txt") is covered up to the hostname. Splitting
// on labels is what keeps that safe: "acme.com" is not a label prefix of
// "acme.community", so an unrelated name is never touched.
func (rw *rewriter) wildcardPrefix(host string) (fiction string, n int, ok bool) {
	for end := len(host); end > 0; end = strings.LastIndexByte(host[:end], '.') {
		if f, hit := rw.wildcardFiction(host[:end]); hit {
			return f, end, true
		}
	}

	return "", 0, false
}

// wildcardFiction returns the fiction covering host, which must already be
// lowercased. Wildcards are tried in mapping order, so the longest suffix
// registered first still wins.
func (rw *rewriter) wildcardFiction(host string) (string, bool) {
	if rw.explicit[host] {
		return "", false
	}

	for _, wc := range rw.wildcards {
		if host == wc.base {
			continue
		}

		if strings.HasSuffix(host, wc.suffix) {
			return wc.fiction, true
		}
	}

	return "", false
}

// mappingKey identifies a mapping by type and real value.
func mappingKey(typ, realVal string) string {
	return typ + "|" + realVal
}

// mappingKeys returns the key of every mapping in engDir. Callers testing many
// candidates use it to read mappings.conf once instead of once per candidate.
func mappingKeys(engDir string) map[string]bool {
	keys := map[string]bool{}

	for _, m := range loadMappings(engDir) {
		keys[mappingKey(m.Type, m.Real)] = true
	}

	return keys
}

// lockAllocation serialises allocation in engDir. Checking that a value has no
// mapping and appending one must be a single step: run apart, concurrent
// requests naming the same new target each gave it a different fiction.
func lockAllocation(engDir string) func() {
	return acquireFileLock(filepath.Join(engDir, "allocate"))
}

// mappingExists compares whole fields. A plain substring search also matched
// mapping *content*, so a custom rule whose fiction text happened to contain
// "domain|evil.com|" made that domain look mapped and it never got one.
func mappingExists(engDir, typ, realVal string) bool {
	return mappingKeys(engDir)[mappingKey(typ, realVal)]
}

// addCustomRule adds a real|fiction rule unless one exists for realVal, and
// reports whether it did.
func addCustomRule(engDir, realVal, fiction string) (bool, error) {
	unlock := lockAllocation(engDir)
	defer unlock()

	if mappingExists(engDir, mapCustom, realVal) {
		return false, nil
	}

	return true, addMapping(engDir, mapCustom, realVal, fiction)
}

var mappingTypes = map[string]bool{
	mapDomain: true, mapWildcard: true, mapOrg: true, mapEmail: true, mapPath: true,
	mapCloud: true, mapIP: true, mapAccount: true, mapCustom: true,
}

// validateMapping rejects values the line format cannot carry. Fields are
// joined with "|", one mapping per line, so a delimiter or a newline in a
// value silently reappears as a different mapping, or as several.
func validateMapping(m mappingEntry) error {
	switch {
	case !mappingTypes[m.Type]:
		return fmt.Errorf("unknown mapping type %q", m.Type)
	case m.Real == "" || m.Fiction == "":
		return fmt.Errorf("mapping %q -> %q has an empty value", m.Real, m.Fiction)
	case strings.ContainsAny(m.Real, "|\r\n"), strings.ContainsAny(m.Fiction, "|\r\n"):
		return fmt.Errorf("mapping %q -> %q contains \"|\" or a line break", m.Real, m.Fiction)
	}

	return nil
}

func addMapping(engDir, typ, realVal, fiction string) error {
	m := mappingEntry{Type: typ, Real: realVal, Fiction: fiction}
	if err := validateMapping(m); err != nil {
		return err
	}

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

// baseLinesFor returns the mappings a base domain gets: the hostname alone
// when it was merely seen, the full derived set when it is a target.
func baseLinesFor(engDir, baseDomain, orgName, baseFiction string, port int, derived bool) ([]string, error) {
	if !derived {
		return []string{fmt.Sprintf("domain|%s|localhost:%d", baseDomain, port)}, nil
	}

	fiction, err := allocateOrgFiction(engDir, baseFiction)
	if err != nil {
		return nil, err
	}

	return baseMappingLines(baseDomain, port, orgName, fiction), nil
}

// baseMappingLines builds the domain mapping and the org, email, path, and
// cloud values derived from it. Every fiction is tied to fictionOrg so distinct
// real orgs reverse to distinct targets instead of colliding.
func baseMappingLines(baseDomain string, port int, orgName, fictionOrg string) []string {
	org := strings.ToLower(orgName)
	fictionLower := strings.ToLower(fictionOrg)

	return []string{
		fmt.Sprintf("domain|%s|localhost:%d", baseDomain, port),
		fmt.Sprintf("org|%s|%s", org, fictionOrg),
		fmt.Sprintf("email|@%s|@%s.local", baseDomain, fictionLower),
		fmt.Sprintf("path|/%s/|/dev/%s/", org, fictionLower),
		fmt.Sprintf("cloud|s3://%s|file:///tmp/%s", org, fictionLower),
	}
}

// allocateOrgFiction returns the next distinct fiction org: the base name for
// the first org, then base+2, base+3, … so no two real orgs share a fiction.
func allocateOrgFiction(engDir, base string) (string, error) {
	n, err := nextCounter(engDir, "org_counter", "1")
	if err != nil {
		return "", err
	}

	if n < 2 {
		return base, nil
	}

	return base + strconv.Itoa(n), nil
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

	next := map[string]int{"port_counter": portNext, "ip_counter": 2, "org_counter": 1, "account_counter": 1}

	for _, m := range loadMappings(engDir) {
		for counter, n := range usedCounters(m) {
			next[counter] = max(next[counter], n+1)
		}
	}

	for _, counter := range slices.Sorted(maps.Keys(next)) {
		if err := writeFileContent(filepath.Join(engDir, counter), strconv.Itoa(next[counter])); err != nil {
			return err
		}
	}

	return nil
}

// usedCounters returns the counter values a mapping's fiction was allocated
// from, keyed by counter file.
func usedCounters(m mappingEntry) map[string]int {
	used := map[string]int{}

	if p, ok := portFromFiction(m.Fiction); ok {
		used["port_counter"] = p
	}

	if n, ok := ipCounterFromFiction(m.Fiction); ok {
		used["ip_counter"] = n
	}

	switch m.Type {
	case mapOrg:
		used["org_counter"] = orgNumberFromFiction(m.Fiction)
	case mapAccount:
		if n, err := strconv.Atoi(m.Fiction); err == nil {
			used["account_counter"] = n
		}
	}

	return used
}

func resolveFictionOrg(engDir, fictionOrg string) string {
	if fictionOrg != "" {
		return fictionOrg
	}

	if stored := readFileContent(filepath.Join(engDir, "fiction_org")); stored != "" {
		return stored
	}

	return defaultFictionOrg
}

// addDomain maps domain as a target: the hostname, plus the org, email, path
// and cloud values derived from it.
func addDomain(engDir, domain, fictionOrg string) error {
	return addDomainMappings(engDir, domain, fictionOrg, true)
}

// addHostOnlyDomain maps just the hostname, for a domain that was seen but is
// not in scope. No org rule is created, so its name keeps its ordinary meaning
// everywhere else in the conversation.
func addHostOnlyDomain(engDir, domain string) error {
	return addDomainMappings(engDir, domain, "", false)
}

func addDomainMappings(engDir, domain, fictionOrg string, derived bool) error {
	// Lowercase so the table has no "Amazon.com"/"amazon.com" duplicates.
	domain = strings.ToLower(strings.TrimRight(domain, "."))

	orgName := extractOrgName(domain)
	if orgName == "" {
		return fmt.Errorf("invalid domain: %s", domain)
	}

	baseFiction := resolveFictionOrg(engDir, fictionOrg)
	baseDomain := extractBaseDomain(domain)
	mappingsPath := filepath.Join(engDir, "mappings.conf")

	unlock := lockAllocation(engDir)
	defer unlock()

	// One snapshot for all three checks: nothing is appended until the end.
	existing := mappingKeys(engDir)

	var lines []string

	if !existing[mappingKey(mapDomain, baseDomain)] {
		port, err := allocatePort(engDir, "domain "+baseDomain)
		if err != nil {
			return err
		}

		base, err := baseLinesFor(engDir, baseDomain, orgName, baseFiction, port, derived)
		if err != nil {
			return err
		}

		lines = append(lines, base...)
	}

	if !existing[mappingKey(mapWildcard, "*."+baseDomain)] {
		port, err := allocatePort(engDir, "wildcard *."+baseDomain)
		if err != nil {
			return err
		}

		lines = append(lines, fmt.Sprintf("wildcard|*.%s|localhost:%d", baseDomain, port))
	}

	if domain != baseDomain && !existing[mappingKey(mapDomain, domain)] {
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

	unlock := lockAllocation(engDir)
	defer unlock()

	if mappingExists(engDir, mapIP, ip) {
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

	return addMapping(engDir, mapIP, ip, fictionIP)
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

// accountRegexes find AWS account IDs where the context says a twelve-digit
// number is one: the account field of an ARN, and an "Account" or "account_id"
// key or variable. A bare twelve-digit number is too common to take.
var accountRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\barn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:(\d{12}):`),
	regexp.MustCompile(`(?i)account(?:[_ -]?id)?["']?\s*[:=]\s*["']?(\d{12})\b`),
}

var awsAccountRegex = regexp.MustCompile(`^\d{12}$`)

// accountFictionPrefix starts every fiction account ID. No real account is
// numbered that low, so scanning tanuki's own output never finds one.
const accountFictionPrefix = "0000000"

// maxFictionAccount is the last counter value that keeps the prefix.
const maxFictionAccount = 99999

func isFictionAccount(id string) bool {
	return strings.HasPrefix(id, accountFictionPrefix)
}

// addAccountMapping gives an AWS account ID a fiction of the same shape, so
// an ARN still reads as an ARN.
func addAccountMapping(engDir, id string) error {
	unlock := lockAllocation(engDir)
	defer unlock()

	if mappingExists(engDir, mapAccount, id) {
		return nil
	}

	n, err := nextCounter(engDir, "account_counter", "1")
	if err != nil {
		return err
	}

	if n > maxFictionAccount {
		return fmt.Errorf("fiction account range exhausted for %s (would leak to upstream)", id)
	}

	return addMapping(engDir, mapAccount, id, fmt.Sprintf("%012d", n))
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
	// Deliberately greedy: findIPs validates each candidate, and
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

// ipMatch is one address found in text: its canonical form and its span.
type ipMatch struct {
	ip         string
	start, end int
}

// findIPs returns the addresses re finds in text. A candidate is trimmed to
// its longest valid prefix, loses a single leading colon ("ip:2001:db8::1"
// otherwise never parsed), and must stand alone -- a letter, digit or "_" on
// either side means it belongs to a longer token, like the "::1234" inside
// "arn:aws:iam::123456789012" or the "1.2.3.45" inside "1.2.3.456".
func findIPs(text string, re *regexp.Regexp) []ipMatch {
	var found []ipMatch

	for _, loc := range re.FindAllStringIndex(text, -1) {
		start := loc[0]
		if strings.HasPrefix(text[start:loc[1]], ":") && !strings.HasPrefix(text[start:loc[1]], "::") {
			start++
		}

		ip, n := longestValidIPPrefix(text[start:loc[1]])
		if n == 0 || !standsAlone(text, start, start+n) {
			continue
		}

		found = append(found, ipMatch{ip: ip, start: start, end: start + n})
	}

	return found
}

// standsAlone reports whether text[start:end] is not glued to a letter, digit
// or "_" on either side.
func standsAlone(text string, start, end int) bool {
	glued := func(r rune) bool { return isAlnum(r) || r == '_' }

	before, _, _ := around(text, start)
	_, after, _ := around(text, end)

	return !glued(before) && !glued(after)
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
	var b strings.Builder

	pos := 0

	for _, m := range findIPs(text, re) {
		fiction, ok := rw.ipFictions[m.ip]
		if !ok {
			continue
		}

		b.WriteString(text[pos:m.start])
		b.WriteString(fiction)

		pos = m.end
	}

	if pos == 0 {
		return text
	}

	b.WriteString(text[pos:])

	return b.String()
}

var domainRegex = regexp.MustCompile(`\b([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}\b`)

func hasCommonTLD(domain string) bool {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return false
	}

	return commonTLDs[strings.ToLower(parts[len(parts)-1])]
}

// isRealDomain reports whether domain ends in an ICANN-managed public suffix
// with a label in front of it. It accepts every real TLD while still rejecting
// code like "config.yaml" or "main.go", whose last element is no suffix at
// all. Used only where hostContext already says the token is a host.
func isRealDomain(domain string) bool {
	suffix, icann := publicsuffix.PublicSuffix(domain)

	return icann && len(suffix) < len(domain)
}

// hostContext reports whether text[start:end] sits where only a hostname
// does: after "://" or "@", or with a path right after it. A bare token on an
// uncommon TLD is far more often code than a domain, so it needs one of these
// markers.
func hostContext(text string, start, end int) bool {
	if pre := text[:start]; strings.HasSuffix(pre, "://") || strings.HasSuffix(pre, "@") {
		return true
	}

	if end >= len(text) {
		return false
	}

	switch text[end] {
	case '/':
		return true
	case ':':
		return portThenPath(text, end+1)
	default:
		return false
	}
}

// portThenPath reports whether text from i is a port followed by a path. The
// path is what makes it a port: ".md", ".py", ".rs" and ".sh" are real TLDs,
// so "README.md:3" and "handlers.py:91" are hostname-and-port by every other
// measure, and dev output is full of them.
func portThenPath(text string, i int) bool {
	digits := 0

	for i+digits < len(text) && digits <= 5 {
		if text[i+digits] < '0' || text[i+digits] > '9' {
			break
		}

		digits++
	}

	return digits > 0 && digits <= 5 && i+digits < len(text) && text[i+digits] == '/'
}

// scanned is what scanTargets found in one string.
type scanned struct{ domains, ips, accounts []string }

var targetScans memoCache[scanned]

// targetsIn returns the distinct real-looking values in text: domains on a
// common TLD, lowercased, public IPs in canonical form, and AWS account IDs.
// It depends on text alone, so long strings are scanned once (see memoCache);
// callers apply the ignore list, which can change, and must not modify the
// slices.
func targetsIn(text string) scanned {
	return targetScans.get(text, scanTargets)
}

func scanTargets(text string) scanned {
	var s scanned

	seen := make(map[string]bool)

	for _, loc := range domainRegex.FindAllStringIndex(text, -1) {
		domain := strings.ToLower(text[loc[0]:loc[1]])
		if seen[domain] {
			continue
		}

		// A common TLD stands on its own; any other real TLD needs the
		// surroundings to say the token is a host, not an attribute access.
		if hasCommonTLD(domain) || (isRealDomain(domain) && hostContext(text, loc[0], loc[1])) {
			seen[domain] = true
			s.domains = append(s.domains, domain)
		}
	}

	for _, m := range slices.Concat(findIPs(text, ipv4Regex), findIPs(text, ipv6Regex)) {
		if !isPrivateIP(m.ip) && !seen[m.ip] {
			seen[m.ip] = true
			s.ips = append(s.ips, m.ip)
		}
	}

	for _, re := range accountRegexes {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if id := m[1]; !isFictionAccount(id) && !seen[id] {
				seen[id] = true
				s.accounts = append(s.accounts, id)
			}
		}
	}

	return s
}

// findNewTargets returns the domains, public IPs and AWS account IDs in texts
// that have no mapping yet, reading mappings.conf once for all of them.
func findNewTargets(engDir string, texts []string, extraIgnored ...string) scanned {
	existing := mappingKeys(engDir)
	emitted := make(map[string]bool)
	found := scanned{domains: []string{}, ips: []string{}, accounts: []string{}}

	for _, text := range texts {
		s := targetsIn(text)

		for _, domain := range s.domains {
			if isIgnoredDomain(domain, extraIgnored...) {
				continue
			}

			// Base domain first so it gets the lower port and owns the derived
			// org/email/path mappings; emitted dedupes the apex-only case.
			for _, candidate := range []string{extractBaseDomain(domain), domain} {
				if !emitted[candidate] && !existing[mappingKey(mapDomain, candidate)] {
					emitted[candidate] = true
					found.domains = append(found.domains, candidate)
				}
			}
		}

		for _, ip := range s.ips {
			if !emitted[ip] && !existing[mappingKey(mapIP, ip)] {
				emitted[ip] = true
				found.ips = append(found.ips, ip)
			}
		}

		for _, id := range s.accounts {
			if !emitted[id] && !existing[mappingKey(mapAccount, id)] {
				emitted[id] = true
				found.accounts = append(found.accounts, id)
			}
		}
	}

	return found
}

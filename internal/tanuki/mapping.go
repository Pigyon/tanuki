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
	"unicode"
	"unicode/utf8"

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
	// fictionLabels holds the hostname labels this engagement's own output is
	// built from, so the gate does not read them back as real targets.
	fictionLabels map[string]bool
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
		rw.fictionLabels = fictionLabelsOf(mappings)
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
		// Wildcard fictions are never reversed. Account fictions are all
		// zeroes and digits, so their prefixes ("0", "00", ...) would hold
		// back the end of every number the model writes.
		if m.Type == mapWildcard || m.Type == mapAccount {
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

// reservedOrgNames are never spent on an org rule by auto-detection. An org
// rule replaces a bare word everywhere it appears, and these are words code is
// made of -- ".net", ".io", ".at", ".network", ".global" and ".online" are all
// real TLDs, so "using System.Net;", "self.network" and "buffer.at(i)" all
// read as hostnames. Mapping one rewrote the word for the rest of the
// engagement and the model was shown mangled source from then on.
//
// "tanuki add" and an in-scope entry both bypass this: the operator saying a
// domain is a target outranks the guess.
var reservedOrgNames = map[string]bool{
	// Receivers, scopes and language keywords.
	"self": true, "this": true, "super": true, "window": true, "global": true,
	"module": true, "exports": true, "package": true, "namespace": true,
	// Objects a program names constantly.
	"console": true, "process": true, "buffer": true, "socket": true,
	"request": true, "response": true, "session": true, "context": true,
	"options": true, "option": true, "config": true, "settings": true,
	"params": true, "args": true, "props": true, "state": true, "system": true, "user": true, "users": true, "admin": true, "client": true,
	"server": true, "object": true, "instance": true, "template": true,
	"metadata": true, "labels": true, "trace": true, "span": true,
	"stack": true, "chars": true, "model": true, "schema": true,
	"result": true, "error": true, "errors": true, "event": true, "events": true,
	"test": true, "tests": true, "spec": true, "mock": true, "fixture": true,
	"util": true, "utils": true, "helper": true, "helpers": true,
}

// worthAnOrgRule reports whether a detected domain's registrable name is
// specific enough to rewrite as a bare word. Length is deliberately not part
// of it: a short name is still a real target ("bit.ly", "ge.com"), and the
// automaton already requires a whole word for one.
func worthAnOrgRule(orgName string) bool {
	return orgName != "" && !reservedOrgNames[strings.ToLower(orgName)]
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

// fictionLabelsOf collects the labels tanuki's own fiction is written from.
func fictionLabelsOf(mappings []mappingEntry) map[string]bool {
	labels := map[string]bool{"localhost": true}

	for _, m := range mappings {
		if m.Type == mapOrg {
			labels[strings.ToLower(m.Fiction)] = true
		}
	}

	return labels
}

// isOwnFiction reports whether a real-looking value is something tanuki wrote.
// An ignored domain still has its org label rewritten, so "acme.com.br" leaves
// as "DEVTARGET.com.br", which the recognisers then read as a real domain --
// and the gate refused the body, naming a fiction string as the leak.
func (rw *rewriter) isOwnFiction(value string) bool {
	labels := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return r == '.' || r == ':'
	})

	// Only the first label is tanuki's: its own output reads
	// "DEVTARGET.com.br" or "localhost:9000", fiction first and whatever the
	// rewrite left behind after it. Matching any label meant a fiction org
	// named after a common suffix -- TANUKI_FICTION_ORG=com -- suppressed
	// every real ".com" residual and turned the gate off.
	return len(labels) > 0 && rw.fictionLabels[labels[0]]
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
func lockAllocation(engDir string) (func(), error) {
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
	unlock, err := lockAllocation(engDir)
	if err != nil {
		return false, err
	}

	defer unlock()

	if mappingExists(engDir, mapCustom, realVal) {
		return false, nil
	}

	if err := addMapping(engDir, mapCustom, realVal, fiction); err != nil {
		return false, err
	}

	return true, nil
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
	case rewritesIntoItself(m):
		return fmt.Errorf("mapping %q -> %q: the fiction still holds the real value", m.Real, m.Fiction)
	}

	return nil
}

// rewritesIntoItself reports whether the real value would still be found
// inside its own replacement, which leaves the gate refusing every body that
// mentions it for the rest of the engagement -- "internal" -> "internal-dev".
//
// The test uses the bound the mapping is matched with, not a plain substring
// search: the org "ge" sits inside the fiction "DEVTARGET", but no
// word-bounded rule can match it there, so that mapping is sound.
func rewritesIntoItself(m mappingEntry) bool {
	machine := newACMachine([]string{m.Real}, []string{m.Fiction}, []boundKind{mappingBound(m.Type)})

	if m.Type == mapOrg {
		machine = newOrgMachine([]termEntry{{From: m.Real, To: m.Fiction}})
	}

	return machine != nil && len(machine.find(m.Fiction)) > 0
}

func addMapping(engDir, typ, realVal, fiction string) error {
	return appendMappings(engDir, []mappingEntry{{Type: typ, Real: realVal, Fiction: fiction}})
}

func nextCounter(engDir, filename, defaultVal string) (int, error) {
	path := filepath.Join(engDir, filename)

	unlock, err := acquireFileLock(path)
	if err != nil {
		return 0, err
	}

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

func hostEntry(domain string, port int) mappingEntry {
	return mappingEntry{mapDomain, domain, fmt.Sprintf("localhost:%d", port)}
}

// derivedMappingLines builds the org, email, path and cloud values a target
// gets beyond its hostname. Every fiction is tied to fictionOrg so distinct
// real orgs reverse to distinct targets instead of colliding.
func derivedMappingEntries(baseDomain, orgName, fictionOrg string) []mappingEntry {
	org := strings.ToLower(orgName)
	fictionLower := strings.ToLower(fictionOrg)

	return []mappingEntry{
		{mapOrg, org, fictionOrg},
		{mapEmail, "@" + baseDomain, "@" + fictionLower + ".local"},
		{mapPath, "/" + org + "/", "/" + fictionLower + "/"},
		{mapCloud, "s3://" + org, "file:///tmp/" + fictionLower},
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

// minFictionOrgLen keeps a fiction org from being a fragment that appears
// everywhere; it is also the first label of every residual tanuki suppresses.
const minFictionOrgLen = 4

// validateFictionOrg rejects a name that cannot safely stand in for an org:
// one the line format cannot carry, one short enough to collide with ordinary
// words, or a public suffix, which would make the gate ignore every residual
// ending in it.
func validateFictionOrg(name string) error {
	switch {
	case len(name) < minFictionOrgLen:
		return fmt.Errorf("fiction org %q is too short (need %d characters)", name, minFictionOrgLen)
	case strings.ContainsAny(name, "|\r\n"), strings.ContainsAny(name, " \t"):
		return fmt.Errorf("fiction org %q contains whitespace, \"|\" or a line break", name)
	case strings.EqualFold(name, "localhost"):
		return fmt.Errorf("fiction org %q is the host every fiction address uses", name)
	}

	if suffix, icann := publicsuffix.PublicSuffix(strings.ToLower(name)); icann && suffix == strings.ToLower(name) {
		return fmt.Errorf("fiction org %q is a public suffix", name)
	}

	return nil
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

// registrableName returns the label a domain's org, path and cloud fictions
// are built from, refusing anything that is not a hostname. A bare word used
// to produce a full set of mappings, so "tanuki add acme.com extra" told the
// operator two targets were protected when "extra" was not one.
func registrableName(domain string) (string, error) {
	org := extractOrgName(domain)
	if org == "" || !strings.Contains(domain, ".") || !domainRegex.MatchString(domain) {
		return "", fmt.Errorf("not a domain: %q", domain)
	}

	return org, nil
}

func addDomainMappings(engDir, domain, fictionOrg string, derived bool) error {
	// Lowercase so the table has no "Amazon.com"/"amazon.com" duplicates.
	domain = strings.ToLower(strings.TrimRight(domain, "."))

	orgName, err := registrableName(domain)
	if err != nil {
		return err
	}

	baseFiction := resolveFictionOrg(engDir, fictionOrg)
	baseDomain := extractBaseDomain(domain)

	unlock, err := lockAllocation(engDir)
	if err != nil {
		return err
	}

	defer unlock()

	// One snapshot for all three checks: nothing is appended until the end.
	existing := mappingKeys(engDir)

	var entries []mappingEntry

	if !existing[mappingKey(mapDomain, baseDomain)] {
		port, err := allocatePort(engDir, "domain "+baseDomain)
		if err != nil {
			return err
		}

		entries = append(entries, hostEntry(baseDomain, port))
	}

	// Checked on its own so a domain already mapped by hostname can still be
	// told it is a target later: a subdomain in scope whose apex is not used
	// to find the apex mapped, skip the whole block, and leave the org, email,
	// path and cloud values uncreated -- which the gate does not catch,
	// because it only looks for values that have a mapping.
	if derived && !existing[mappingKey(mapOrg, strings.ToLower(orgName))] {
		fiction, err := allocateOrgFiction(engDir, baseFiction)
		if err != nil {
			return err
		}

		entries = append(entries, derivedMappingEntries(baseDomain, orgName, fiction)...)
	}

	if !existing[mappingKey(mapWildcard, "*."+baseDomain)] {
		port, err := allocatePort(engDir, "wildcard *."+baseDomain)
		if err != nil {
			return err
		}

		entries = append(entries, mappingEntry{mapWildcard, "*." + baseDomain, fmt.Sprintf("localhost:%d", port)})
	}

	if domain != baseDomain && !existing[mappingKey(mapDomain, domain)] {
		port, err := allocatePort(engDir, "subdomain "+domain)
		if err != nil {
			return err
		}

		entries = append(entries, hostEntry(domain, port))
	}

	return appendMappings(engDir, entries)
}

// appendMappings validates every entry before writing any of them, so a value
// the line format cannot carry is refused rather than shredding the table.
// addDomainMappings built its lines by hand and wrote them straight out, which
// is how "--fiction-org ACME-CORP" (a fiction containing its own real value)
// and a fiction org holding a newline both got in.
func appendMappings(engDir string, entries []mappingEntry) error {
	if len(entries) == 0 {
		return nil
	}

	lines := make([]string, 0, len(entries))

	for _, m := range entries {
		if err := validateMapping(m); err != nil {
			return err
		}

		lines = append(lines, fmt.Sprintf("%s|%s|%s", m.Type, m.Real, m.Fiction))
	}

	if err := checkTableConflicts(loadMappings(engDir), entries); err != nil {
		return err
	}

	return appendToFile(filepath.Join(engDir, "mappings.conf"), strings.Join(lines, "\n"))
}

// rewritePass is the order the rewriter applies a mapping in: the automaton
// first, org rules after it. A value the automaton introduces is still seen by
// the org pass, so it is not a conflict; one introduced after its own pass has
// run survives into the body.
func rewritePass(typ string) int {
	if typ == mapOrg {
		return 1
	}

	return 0
}

// checkTableConflicts refuses an addition that would leave a mapped value in
// the rewritten body, which makes the gate refuse every body naming it -- for
// the rest of the engagement, since Claude Code resends history.
//
// Only a value no later pass will clean up counts. Modelling that matters:
// without it, "tanuki add widgetco.com" was refused because an unrelated
// custom rule's fiction said "WidgetCo", which the org pass rewrites anyway.
//
// The error names no value. It reaches the client, which folds it into the
// next prompt, and a bare org name is not domain-shaped so nothing downstream
// would catch it.
func checkTableConflicts(existing, added []mappingEntry) error {
	all := slices.Concat(existing, added)

	for _, m := range added {
		if found := residualTargets(nil, m.Fiction); len(found) > 0 {
			logger.Warn("refusing a fiction the leak check would flag",
				"type", m.Type, "fiction", m.Fiction, "looks_like", found)

			return fmt.Errorf("the fiction for this %s mapping looks like a real target; "+
				"the gate would refuse every body naming it (see the tanuki log)", m.Type)
		}
	}

	// Only org and custom values are what the gate scans a rewritten body for
	// (see survivors), so only an addition of one can create this conflict.
	// Skipping otherwise keeps the auto-map path off an O(table) check it can
	// run thousands of times in one body.
	if !slices.ContainsFunc(added, func(m mappingEntry) bool {
		return m.Type == mapOrg || m.Type == mapCustom
	}) {
		return nil
	}

	machine, owners := newConflictMachine(all)
	if machine == nil {
		return nil
	}

	for _, m := range all {
		isNew := slices.Contains(added, m)

		for _, hit := range machine.find(m.Fiction) {
			held := owners[hit.index]

			sameValue := strings.EqualFold(held.Real, m.Real)
			if sameValue || rewritePass(held.Type) > rewritePass(m.Type) {
				continue
			}

			if !isNew && !slices.ContainsFunc(added, func(a mappingEntry) bool {
				return strings.EqualFold(a.Real, held.Real)
			}) {
				continue
			}

			logger.Warn("refusing a mapping whose fiction holds another mapped value",
				"real", m.Real, "fiction", m.Fiction, "holds", held.Real)

			return fmt.Errorf("this %s mapping's fiction holds another mapped value; "+
				"the gate would refuse every body naming it (see the tanuki log)", m.Type)
		}
	}

	return nil
}

// newConflictMachine matches every mapped real value with the bound its own
// mapping uses, and reports which mapping each pattern came from.
func newConflictMachine(all []mappingEntry) (*acMachine, []mappingEntry) {
	var (
		patterns []string
		bounds   []boundKind
		owners   []mappingEntry
	)

	for _, m := range all {
		// The gate scans a rewritten body for these two only.
		if m.Type != mapOrg && m.Type != mapCustom || m.Real == "" {
			continue
		}

		bound := mappingBound(m.Type)
		if m.Type == mapOrg {
			bound = boundWord
			if len(m.Real) >= minOrgPartLen {
				bound = boundWordPart
			}
		}

		patterns = append(patterns, m.Real)
		bounds = append(bounds, bound)
		owners = append(owners, m)
	}

	if len(patterns) == 0 {
		return nil, nil
	}

	return newACMachine(patterns, patterns, bounds), owners
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

	unlock, err := lockAllocation(engDir)
	if err != nil {
		return err
	}

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

	if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsUnspecified() ||
		parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() ||
		parsed.IsMulticast() || parsed.IsInterfaceLocalMulticast() {
		return true
	}

	return isSpecialPurposeV4(parsed)
}

// cgnatRange is RFC 6598 shared address space, which is carrier equipment
// rather than a target.
var cgnatRange = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isSpecialPurposeV4 rejects addresses that are never a host anyone tests: the
// all-ones broadcast, a netmask, and shared address space. A netmask next to
// an address in ifconfig output was being mapped, so the subnet the model
// needs to reason about came back as 127.0.0.4.
func isSpecialPurposeV4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}

	if v4.Equal(net.IPv4bcast) || cgnatRange.Contains(v4) || v4[0] >= 240 {
		return true
	}

	// A contiguous run of high bits then zeros is a mask, not an address.
	return isContiguousMask(v4)
}

func isContiguousMask(v4 net.IP) bool {
	ones, bits := net.IPMask(v4).Size()

	return bits == 32 && ones > 0 && ones < 32
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
	unlock, err := lockAllocation(engDir)
	if err != nil {
		return err
	}

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

// findIPs returns the addresses re finds in text.
func findIPs(text string, re *regexp.Regexp) []ipMatch {
	var found []ipMatch

	for _, loc := range re.FindAllStringIndex(text, -1) {
		if m, ok := ipAt(text, loc[0], loc[1]); ok {
			found = append(found, m)
		}
	}

	return found
}

// ipAt returns the address inside the span [from,to).
//
// The patterns are greedy and can begin partway into a word: in
// "IPv6:2001:db8::1" the leftmost match starts at the "6" of "IPv6" and covers
// "6:2001:db8::1", itself a valid address. Rejecting the span for that dropped
// the real address from detection and from the gate alike, so each start is
// tried until one is not glued to the end of a word.
//
// A start is viable when the character in front of it is not alphanumeric, so
// punctuation an address is written next to still counts:
// "dump_2001:db8::1.pcap", "[2001:db8::1]", "ip:2001:db8::1". The end must not
// run into a hex digit, which keeps "::1234" out of "iam::123456789012" and
// "1.2.3.45" out of "1.2.3.456".
func ipAt(text string, from, to int) (ipMatch, bool) {
	for start := from; start < to; start++ {
		if start > 0 && isAlnumByte(text[start-1]) {
			continue
		}

		ip, n := longestValidIPPrefix(text[start:to])
		if n == 0 || addressContinuesAt(text, start+n) || isHardwareAddress(text[start:start+n]) {
			continue
		}

		return ipMatch{ip: ip, start: start, end: start + n}, true
	}

	return ipMatch{}, false
}

// precededByVersion reports whether the number at i is introduced by the word
// "version". A four-part version string is shaped exactly like an address, and
// nothing in the number tells them apart, so where the text says which it is,
// take its word.
//
// The word has to end the preceding token, not merely be a suffix of it:
// testing for a trailing "ver" swallowed "Server: 203.0.113.5",
// "resolver 203.0.113.9" and "failover 203.0.113.7", which is the commonest
// shape in nmap, curl and dig output. Compounds ending in "version"
// (AssemblyVersion, FileVersion) still count.
func precededByVersion(text string, i int) bool {
	end := i
	for end > 0 && strings.ContainsRune(" \t\"'>=:.,/(-", rune(text[end-1])) {
		end--
	}

	start := end
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if !unicode.IsLetter(r) {
			break
		}

		start -= size
	}

	word := strings.ToLower(text[start:end])

	return notAnAddressWords[word] || strings.HasSuffix(word, "version")
}

// notAnAddressWords introduce a dotted quad that is not a host: a version, or
// a piece of subnet configuration. "broadcast 192.0.2.255" and
// "netmask 255.255.255.0" are the shape ifconfig and ip addr print, and
// mapping them destroys the subnet the model has to reason about.
var notAnAddressWords = map[string]bool{
	"ver": true, "v": true, "release": true, "kernel": true, "build": true,
	"assembly": true, "revision": true, "rev": true, "версия": true,
	"netmask": true, "mask": true, "genmask": true, "broadcast": true,
	"bcast": true,
}

// isHardwareAddress reports whether raw is a MAC or Fibre Channel WWN rather
// than an address. Eight groups of exactly two hex digits parse as a valid
// IPv6, so storage and network inventory output became targets. A real address
// written that way is indistinguishable, but it is not how one is written: a
// run with no "::" and every group padded to two digits is hardware.
func isHardwareAddress(raw string) bool {
	if strings.Contains(raw, "::") || strings.Contains(raw, ".") {
		return false
	}

	groups := strings.Split(raw, ":")
	if len(groups) < 6 {
		return false
	}

	for _, g := range groups {
		if len(g) != 2 {
			return false
		}
	}

	return true
}

// startsMidWord reports whether i falls inside a word, which the leading \b
// used to rule out for ASCII only.
func startsMidWord(text string, i int) bool {
	if i == 0 {
		return false
	}

	r, _ := utf8.DecodeLastRuneInString(text[:i])

	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isAlnumByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// addressContinuesAt reports whether an address carries on at i, either
// directly or through a separator with another hex digit behind it.
func addressContinuesAt(text string, i int) bool {
	if i >= len(text) {
		return false
	}

	if isHexDigit(text[i]) {
		return true
	}

	isSeparator := text[i] == '.' || text[i] == ':'

	return isSeparator && i+1 < len(text) && isHexDigit(text[i+1])
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

// domainRegex matches a hostname. Labels take Unicode letters so an
// internationalised name is matched whole: an ASCII-only class entered
// "münchen.de" after the non-ASCII rune and detected "nchen.de", which mapped
// the wrong name and left "mü" in the rewritten text.
//
// There is no leading \b: Go's is ASCII-only, so it never fires before a label
// like "例え". startsMidWord does that job for every script.
var domainRegex = regexp.MustCompile(`([\p{L}\p{N}]([\p{L}\p{N}-]*[\p{L}\p{N}])?\.)+\p{L}{2,}\b`)

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
	pre := text[:start]

	if strings.HasSuffix(pre, "://") {
		return true
	}

	// "user@host" is an address; a bare "@host" at the start of a line is a
	// Ruby instance variable or a decorator, and "@order.customer.email" was
	// being mapped as a hostname.
	if strings.HasSuffix(pre, "@") && len(pre) > 1 && isAlnumByte(pre[len(pre)-2]) {
		return true
	}

	if end >= len(text) {
		return false
	}

	switch text[end] {
	case '/':
		// The weakest marker: "gopkg.in/yaml.v3" and "docs.rs/serde" are module
		// paths, so two labels need a scheme or an "@" instead. A digit after
		// the slash is division -- "obj.data.app/2" -- not a path.
		if end+1 < len(text) && text[end+1] >= '0' && text[end+1] <= '9' {
			return false
		}

		return strings.Count(text[start:end], ".") >= 2
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

// hostInMatch returns the hostname inside a regex match, which can be shorter
// than the match itself. Prose that drops the space after a full stop glues
// the next sentence on -- "Our scope is target.com.We enumerated 42 hosts" --
// and the greedy pattern swallows it, so the whole token failed every test and
// the target was missed by detection and by the gate alike.
//
// Trailing labels are dropped one at a time and each prefix retried; a shorter
// prefix has to carry a curated TLD, so "java.net.Socket" does not hand back
// "java.net".
func hostInMatch(text string, from, to int) string {
	full := strings.ToLower(text[from:to])
	if isDetectableHost(text, full, from, to) {
		return full
	}

	for end := strings.LastIndexByte(full, '.') + from; end > from; end = strings.LastIndexByte(text[from:end], '.') + from {
		candidate := strings.ToLower(text[from:end])
		if hasCommonTLD(candidate) && isDetectableHost(text, candidate, from, end) {
			return candidate
		}
	}

	return ""
}

// codeKeywords introduce a module path, not a host.
var codeKeywords = []string{"import ", "from ", "using ", "require ", "package ", "namespace ", "extern "}

// precededByCodeKeyword reports whether the token at start follows a keyword
// that introduces a module path: "from scipy.io import loadmat" names a Python
// module, not a site on Cocos Islands.
func precededByCodeKeyword(text string, start int) bool {
	const lead = 16

	before := strings.ToLower(text[max(0, start-lead):start])

	return slices.ContainsFunc(codeKeywords, func(kw string) bool {
		return strings.HasSuffix(before, kw)
	})
}

// isDetectableHost reports whether the token at [start,end) is a hostname.
//
// A bare token needs a TLD from the curated list. Any other real TLD needs the
// surroundings to say it is a host: after "://" or "@", or with a path right
// after it. Accepting a bare token on any ICANN suffix because it had three or
// more labels was tried and withdrawn -- attribute chains reach real TLDs
// constantly ("module.exports.app", "self.model.fit",
// "spec.template.metadata.labels.app"), and no reading of the surrounding line
// separated them from enumeration output without either mangling source or
// missing the output it was added for. A target on an uncommon TLD needs
// "tanuki add" or a scope entry; "tanuki verify" finds one in a draft report.
//
// The registrable name is deliberately not consulted here. It decides whether
// a target earns an org rule, which rewrites a bare word everywhere, and
// scope.isTarget makes that call. Letting it veto the host decision hid
// "store.com" and "core.ac.uk" from detection and from the gate.
func isDetectableHost(text, domain string, start, end int) bool {
	if precededByCodeKeyword(text, start) {
		return false
	}

	if hostContext(text, start, end) {
		return hasCommonTLD(domain) || isRealDomain(domain)
	}

	return hasCommonTLD(domain) && worthAnOrgRule(extractOrgName(domain))
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
		if startsMidWord(text, loc[0]) {
			continue
		}

		domain := hostInMatch(text, loc[0], loc[1])
		if domain == "" || seen[domain] {
			continue
		}

		seen[domain] = true
		s.domains = append(s.domains, domain)
	}

	for _, m := range slices.Concat(findIPs(text, ipv4Regex), findIPs(text, ipv6Regex)) {
		if precededByVersion(text, m.start) {
			continue
		}

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

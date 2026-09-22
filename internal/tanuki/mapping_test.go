package tanuki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture targets used across the rewriting tests.
const (
	testBaseDomain = "amazon.com"
	testSubDomain  = "bounty.amazon.com"
	testOrgName    = "amazon"
	testPublicIP   = "52.94.236.248"

	// Fiction values the fixtures allocate, in allocation order: the base
	// domain takes the first port and its wildcard the second.
	testFictionBase     = "localhost:9000"
	testFictionWildcard = "localhost:9001"
)

// newTestEngagement creates an isolated engagement directory seeded with
// the given domains and returns its path.
func newTestEngagement(t *testing.T, domains ...string) string {
	t.Helper()

	engDir := t.TempDir()

	if err := writeFileContent(filepath.Join(engDir, "fiction_org"), "DEVTARGET"); err != nil {
		t.Fatalf("seeding fiction_org: %v", err)
	}

	if err := writeFileContent(filepath.Join(engDir, "port_counter"), "9000"); err != nil {
		t.Fatalf("seeding port_counter: %v", err)
	}

	if err := writeFileContent(filepath.Join(engDir, "ip_counter"), "2"); err != nil {
		t.Fatalf("seeding ip_counter: %v", err)
	}

	if err := writeFileContent(filepath.Join(engDir, "mappings.conf"), "# test\n"); err != nil {
		t.Fatalf("seeding mappings.conf: %v", err)
	}

	for _, d := range domains {
		if err := addDomain(engDir, d, "DEVTARGET"); err != nil {
			t.Fatalf("addDomain(%q): %v", d, err)
		}
	}

	return engDir
}

func rewriters(t *testing.T, engDir string) (r2f, f2r *rewriter) {
	t.Helper()

	mappings := loadMappings(engDir)
	if len(mappings) == 0 {
		t.Fatal("no mappings loaded")
	}

	return newRewriter(mappings, directionR2F), newRewriter(mappings, directionF2R)
}

// TestNoRealValueSurvivesRewrite is the core OpSec assertion: after the
// real->fiction pass, no real target string may remain in the text. A
// dropped mapping here means real data reaches the upstream API.
func TestNoRealValueSurvivesRewrite(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)
	r2f, _ := rewriters(t, engDir)

	leaky := []string{
		testBaseDomain,
		testSubDomain,
		"admin@amazon.com",
		"@amazon.com",
		"s3://amazon",
		"/amazon/",
	}

	texts := []string{
		"check bounty.amazon.com for issues",
		"curl https://bounty.amazon.com/api/v1/users",
		"email admin@amazon.com about the finding",
		"the amazon.com origin resolves to the bounty.amazon.com host",
		"AMAZON and Amazon and amazon in one line",
		"files under /amazon/ and bucket s3://amazon",
		"nested: sub.bounty.amazon.com",
	}

	for _, text := range texts {
		got := r2f.rewrite(text)

		for _, real := range leaky {
			if strings.Contains(got, real) {
				t.Errorf("real value %q leaked\n  input:  %s\n  output: %s", real, text, got)
			}
		}
	}
}

func TestRoundTripRestoresRealValues(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)
	r2f, f2r := rewriters(t, engDir)

	tests := []struct {
		name  string
		input string
	}{
		{name: "bare domain", input: testBaseDomain},
		{name: "subdomain", input: testSubDomain},
		{name: "url", input: "https://bounty.amazon.com/api"},
		{name: "email", input: "admin@amazon.com"},
		{name: "mixed", input: "scan bounty.amazon.com then mail admin@amazon.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fiction := r2f.rewrite(tt.input)
			if fiction == tt.input {
				t.Fatalf("nothing was rewritten: %q", tt.input)
			}

			if got := f2r.rewrite(fiction); got != tt.input {
				t.Errorf("round-trip mismatch\n  want: %s\n  got:  %s\n  via:  %s", tt.input, got, fiction)
			}
		})
	}
}

// TestLongestMatchWins guards the Aho-Corasick selection: a subdomain
// must not be rewritten as its base domain plus a leftover suffix.
func TestLongestMatchWins(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)

	mappings := loadMappings(engDir)

	var baseFiction, subFiction string

	for _, m := range mappings {
		switch {
		case m.Type == "domain" && m.Real == testBaseDomain:
			baseFiction = m.Fiction
		case m.Type == "domain" && m.Real == testSubDomain:
			subFiction = m.Fiction
		}
	}

	if baseFiction == "" || subFiction == "" {
		t.Fatalf("expected both domain mappings, got base=%q sub=%q", baseFiction, subFiction)
	}

	if baseFiction == subFiction {
		t.Fatalf("base and subdomain share fiction value %q", baseFiction)
	}

	r2f := newRewriter(mappings, directionR2F)

	got := r2f.rewrite(testSubDomain)
	if got != subFiction {
		t.Errorf("subdomain rewrote to %q, want %q (longest match lost)", got, subFiction)
	}

	if strings.Contains(got, "bounty.") {
		t.Errorf("partial rewrite left a real prefix: %q", got)
	}
}

func TestWildcardRewriting(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain)

	mappings := loadMappings(engDir)

	var wildcardFiction string

	for _, m := range mappings {
		if m.Type == "wildcard" && m.Real == "*.amazon.com" {
			wildcardFiction = m.Fiction
		}
	}

	if wildcardFiction == "" {
		t.Fatal("no wildcard mapping created for *.amazon.com")
	}

	r2f, _ := rewriters(t, engDir)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "arbitrary subdomain", input: "api.amazon.com", expected: wildcardFiction},
		{name: "deep subdomain", input: "a.b.c.amazon.com", expected: wildcardFiction},
		{
			name:     "in sentence",
			input:    "check staging.amazon.com endpoint",
			expected: "check " + wildcardFiction + " endpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := r2f.rewrite(tt.input); got != tt.expected {
				t.Errorf("rewrite(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}

	// The base domain itself must NOT match the wildcard — it has its own mapping.
	base := r2f.rewrite(testBaseDomain)
	if base == wildcardFiction {
		t.Errorf("base domain matched wildcard instead of its own mapping")
	}
}

// TestUnmappedSubdomainLabelDoesNotLeak is a regression test. The wildcard
// pass used to run after the exact-match pass, so an unmapped subdomain had
// its base rewritten in place ("internal-admin.amazon.com" ->
// "internal-admin.localhost:9000") and the label reached the upstream API.
func TestUnmappedSubdomainLabelDoesNotLeak(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain)
	r2f, _ := rewriters(t, engDir)

	labels := []string{"internal-admin", "staging-vpn", "jira", "vpn-gateway"}

	for _, label := range labels {
		got := r2f.rewrite("connect to " + label + ".amazon.com now")

		if strings.Contains(got, label) {
			t.Errorf("subdomain label %q leaked: %s", label, got)
		}

		if strings.Contains(got, testOrgName) {
			t.Errorf("base domain leaked for %q: %s", label, got)
		}
	}
}

// TestExplicitSubdomainBeatsWildcard ensures the wildcard does not swallow a
// subdomain that has its own mapping, which would break round-tripping.
func TestExplicitSubdomainBeatsWildcard(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)
	r2f, f2r := rewriters(t, engDir)

	var subFiction string

	for _, m := range loadMappings(engDir) {
		if m.Type == "domain" && m.Real == testSubDomain {
			subFiction = m.Fiction
		}
	}

	if got := r2f.rewrite(testSubDomain); got != subFiction {
		t.Fatalf("explicit subdomain rewrote to %q, want its own mapping %q", got, subFiction)
	}

	if got := f2r.rewrite(subFiction); got != testSubDomain {
		t.Errorf("explicit subdomain did not round-trip: got %q", got)
	}
}

// TestCaseInsensitiveMatching is a regression test for the case-sensitivity
// leak class. The old exact-case automaton only matched the one casing a
// value was stored in, so a real target written as "AMAZON.COM", "aMaZoN", or
// "API.AMAZON.COM" sailed through to the upstream API unrewritten.
func TestCaseInsensitiveMatching(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)
	r2f, _ := rewriters(t, engDir)

	cases := []struct {
		name  string
		input string
		leaks []string // must not survive, compared case-insensitively
	}{
		{name: "uppercase domain", input: "visit AMAZON.COM now", leaks: []string{testOrgName}},
		{name: "mixed domain", input: "visit Amazon.Com now", leaks: []string{testOrgName}},
		{name: "explicit subdomain uppercase", input: "BOUNTY.AMAZON.COM", leaks: []string{testOrgName, "bounty"}},
		{name: "wildcard subdomain uppercase", input: "hit API.AMAZON.COM here", leaks: []string{testOrgName, "api."}},
		{name: "org arbitrary case", input: "the aMaZoN team", leaks: []string{testOrgName}},
		{name: "email uppercase", input: "mail ADMIN@AMAZON.COM", leaks: []string{testOrgName}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := r2f.rewrite(tc.input)
			lower := strings.ToLower(got)

			for _, leak := range tc.leaks {
				if strings.Contains(lower, leak) {
					t.Errorf("real value %q leaked case-insensitively\n  input:  %s\n  output: %s", leak, tc.input, got)
				}
			}
		})
	}
}

func TestPrivateIPsAreNotMapped(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	private := []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "0.0.0.0"}

	for _, ip := range private {
		if err := addIPMapping(engDir, ip); err != nil {
			t.Fatalf("addIPMapping(%q): %v", ip, err)
		}

		if mappingExists(engDir, "ip", ip) {
			t.Errorf("private IP %q was mapped, wasting fiction space", ip)
		}
	}
}

func TestPublicIPsAreMappedAndRoundTrip(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	publicIPs := []string{testPublicIP, "8.8.8.8", "2001:4860:4860::8888"}

	for _, ip := range publicIPs {
		if err := addIPMapping(engDir, ip); err != nil {
			t.Fatalf("addIPMapping(%q): %v", ip, err)
		}

		if !mappingExists(engDir, "ip", ip) {
			t.Fatalf("public IP %q was not mapped", ip)
		}
	}

	r2f, f2r := rewriters(t, engDir)

	for _, ip := range publicIPs {
		text := "connect to " + ip + " now"

		fiction := r2f.rewrite(text)
		if strings.Contains(fiction, ip) {
			t.Errorf("real IP %q survived rewrite: %s", ip, fiction)
		}

		if got := f2r.rewrite(fiction); got != text {
			t.Errorf("IP round-trip mismatch for %q: got %q", ip, got)
		}
	}
}

// TestTLDDetectionAvoidsCodePatterns pins the false-positive guard: code
// tokens that look like domains must not become mappings, or ordinary
// source discussions get corrupted.
func TestTLDDetectionAvoidsCodePatterns(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	tests := []struct {
		input    string
		expected bool // should a new domain be detected?
	}{
		{input: "readme.md", expected: false},
		{input: "config.yaml", expected: false},
		{input: "user.id", expected: false},
		{input: "foo.bar", expected: false},
		{input: "main.go", expected: false},
		{input: "styles.css", expected: false},
		{input: "see target.com for details", expected: true},
		{input: "api.acme.io is live", expected: true},
	}

	for _, tt := range tests {
		got := len(findNewDomains(tt.input, engDir)) > 0
		if got != tt.expected {
			t.Errorf("findNewDomains(%q) detected=%v, want %v", tt.input, got, tt.expected)
		}
	}
}

// TestFindNewDomainsDetectsApexDomains is a regression test: a prompt
// mentioning only a bare apex domain used to produce no mapping at all,
// so the real domain was sent to the upstream API unrewritten.
func TestFindNewDomainsDetectsApexDomains(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	tests := []struct {
		input    string
		expected []string
	}{
		{input: "check amazon.com for issues", expected: []string{testBaseDomain}},
		{input: "check bounty.amazon.com", expected: []string{testBaseDomain, testSubDomain}},
		{input: "amazon.com and amazon.com again", expected: []string{testBaseDomain}},
		{input: "both acme.io and acme.io/path", expected: []string{"acme.io"}},
	}

	for _, tt := range tests {
		got := findNewDomains(tt.input, engDir)
		if len(got) != len(tt.expected) {
			t.Errorf("findNewDomains(%q) = %v, want %v", tt.input, got, tt.expected)
			continue
		}

		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("findNewDomains(%q) = %v, want %v", tt.input, got, tt.expected)
				break
			}
		}
	}
}

func TestFindNewDomainsSkipsKnownMappings(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain)

	if found := findNewDomains("visit amazon.com", engDir); len(found) != 0 {
		t.Errorf("already-mapped domain re-detected: %v", found)
	}

	found := findNewDomains("visit bounty.amazon.com", engDir)
	if len(found) != 1 || found[0] != testSubDomain {
		t.Errorf("new subdomain not detected, got %v", found)
	}
}

func TestTerminologyIsRewrittenOneWay(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")
	r2f, f2r := rewriters(t, engDir)

	if len(terminology) == 0 {
		t.Fatal("terminology dictionary is empty")
	}

	term := terminology[0]

	got := r2f.rewrite("we found a " + term.From + " here")
	if strings.Contains(strings.ToLower(got), strings.ToLower(term.From)) {
		t.Errorf("term %q was not rewritten: %s", term.From, got)
	}

	// f2r must not translate dev terms back, or ordinary words in model
	// output would be turned into pentest jargon.
	back := f2r.rewrite(got)
	if strings.Contains(strings.ToLower(back), strings.ToLower(term.From)) {
		t.Errorf("terminology was reversed by f2r: %s", back)
	}
}

func TestSingleWordTermsAreNotRewritten(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")
	r2f, _ := rewriters(t, engDir)

	text := "the target payload exploit shell code"
	if got := r2f.rewrite(text); got != text {
		t.Errorf("ambiguous single words were rewritten\n  want: %s\n  got:  %s", text, got)
	}
}

func TestCustomRuleRoundTrip(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	if err := addMapping(engDir, "custom", "John Smith", "Jane Developer"); err != nil {
		t.Fatalf("addMapping: %v", err)
	}

	r2f, f2r := rewriters(t, engDir)

	text := "ask John Smith about it"

	fiction := r2f.rewrite(text)
	if strings.Contains(fiction, "John Smith") {
		t.Errorf("custom rule did not apply: %s", fiction)
	}

	if got := f2r.rewrite(fiction); got != text {
		t.Errorf("custom rule round-trip mismatch: got %q", got)
	}
}

func TestExtractOrgAndBaseDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input        string
		expectedBase string
		expectedOrg  string
	}{
		{input: testBaseDomain, expectedBase: testBaseDomain, expectedOrg: testOrgName},
		{input: testSubDomain, expectedBase: testBaseDomain, expectedOrg: testOrgName},
		{input: "a.b.c.amazon.com", expectedBase: testBaseDomain, expectedOrg: testOrgName},
		{input: "example.co.uk", expectedBase: "example.co.uk", expectedOrg: "example"},
		{input: "api.example.co.uk", expectedBase: "example.co.uk", expectedOrg: "example"},
	}

	for _, tt := range tests {
		if got := extractBaseDomain(tt.input); got != tt.expectedBase {
			t.Errorf("extractBaseDomain(%q) = %q, want %q", tt.input, got, tt.expectedBase)
		}

		if got := extractOrgName(tt.input); got != tt.expectedOrg {
			t.Errorf("extractOrgName(%q) = %q, want %q", tt.input, got, tt.expectedOrg)
		}
	}
}

func TestAddDomainIsIdempotent(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain)

	before := len(loadMappings(engDir))

	if err := addDomain(engDir, testBaseDomain, "DEVTARGET"); err != nil {
		t.Fatalf("addDomain: %v", err)
	}

	if after := len(loadMappings(engDir)); after != before {
		t.Errorf("re-adding a domain changed mapping count: %d -> %d", before, after)
	}
}

func TestLoadMappingsIgnoresCommentsAndBlanks(t *testing.T) {
	t.Parallel()

	engDir := t.TempDir()

	content := strings.Join([]string{
		"# a comment",
		"",
		"domain|amazon.com|localhost:9000",
		"   ",
		"# another|comment|line",
		"org|Amazon|DEVTARGET",
		"malformed-line-without-pipes",
	}, "\n")

	if err := os.WriteFile(filepath.Join(engDir, "mappings.conf"), []byte(content), 0o600); err != nil {
		t.Fatalf("writing mappings: %v", err)
	}

	mappings := loadMappings(engDir)
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %d: %+v", len(mappings), mappings)
	}
}

// TestImportedCountersAvoidCollision is a regression test: importing a
// mappings.conf that already uses high ports/IPs must advance the counters
// past them, or the next allocation reuses a fiction value and maps two real
// values onto it.
func TestImportedCountersAvoidCollision(t *testing.T) {
	t.Parallel()

	engDir := t.TempDir()

	content := strings.Join([]string{
		"# imported",
		"domain|amazon.com|localhost:9005",
		"wildcard|*.amazon.com|localhost:9006",
		"ip|52.94.236.248|127.0.0.7",
	}, "\n") + "\n"

	writes := map[string]string{
		"mappings.conf": content,
		"port_counter":  "9000", // seed value that would collide
		"ip_counter":    "2",
		"fiction_org":   "DEVTARGET",
	}
	for name, body := range writes {
		if err := writeFileContent(filepath.Join(engDir, name), body); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	if err := resetCountersFromMappings(engDir); err != nil {
		t.Fatalf("resetCountersFromMappings: %v", err)
	}

	if got := readFileContent(filepath.Join(engDir, "port_counter")); got != "9007" {
		t.Errorf("port_counter = %q, want 9007", got)
	}

	if got := readFileContent(filepath.Join(engDir, "ip_counter")); got != "7" {
		t.Errorf("ip_counter = %q, want 7", got)
	}

	// A newly added domain must not reuse a port already in the import.
	if err := addDomain(engDir, "example.org", "DEVTARGET"); err != nil {
		t.Fatalf("addDomain: %v", err)
	}

	for _, m := range loadMappings(engDir) {
		// Real == "example.org" uniquely identifies the domain mapping.
		if m.Real == "example.org" {
			if p, ok := portFromFiction(m.Fiction); ok && p < 9007 {
				t.Errorf("new domain reused an imported port: %s", m.Fiction)
			}
		}
	}
}

// TestMultiOrgFictionsDoNotCollide is a regression test: every real org used to
// map to the single fiction org, so a multi-target engagement reversed both
// targets' org/email/path/ticket to whichever one won the tie.
func TestMultiOrgFictionsDoNotCollide(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, "google.com")
	r2f, f2r := rewriters(t, engDir)

	text := "mail admin@amazon.com and dev@google.com"
	fiction := r2f.rewrite(text)

	if strings.Contains(fiction, testOrgName) || strings.Contains(fiction, "google") {
		t.Errorf("a real org leaked: %s", fiction)
	}

	// Each target must reverse to itself, not to the other org.
	if got := f2r.rewrite(fiction); got != text {
		t.Errorf("multi-org round-trip mismatch\n  want: %s\n  got:  %s", text, got)
	}
}

// TestUncommonButRealTLDsDetected is a regression test: common TLDs like .ai
// were missing from the curated list, so those targets were never mapped and
// leaked upstream.
func TestUncommonButRealTLDsDetected(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	for _, domain := range []string{"evilcorp.ai", "clan.gg", "bit.ly"} {
		if len(findNewDomains(domain, engDir)) == 0 {
			t.Errorf("domain %q was not detected (TLD missing from the list)", domain)
		}
	}
}

// TestIPCanonicalFormsAreRewritten is a regression test for the IP leak class:
// the automaton matched only the exact stored text, so a zero-padded or
// differently-compressed form of a mapped IP reached the API unrewritten.
func TestIPCanonicalFormsAreRewritten(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "example.com")

	for _, ip := range []string{testPublicIP, "2001:db8:85a3::8a2e:370:7334"} {
		if err := addIPMapping(engDir, ip); err != nil {
			t.Fatalf("addIPMapping(%q): %v", ip, err)
		}
	}

	r2f, f2r := rewriters(t, engDir)

	// zero-padded IPv4, uncompressed IPv6, uppercase IPv6, IPv4-mapped IPv6.
	forms := []string{
		"052.094.236.248",
		"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
		"2001:DB8:85A3::8A2E:370:7334",
		"::ffff:52.94.236.248",
	}

	for _, form := range forms {
		got := r2f.rewrite("addr " + form + " end")
		if strings.Contains(got, form) {
			t.Errorf("non-canonical IP form %q leaked: %s", form, got)
		}
	}

	// A canonical mapped IP still round-trips.
	fiction := r2f.rewrite(testPublicIP)
	if back := f2r.rewrite(fiction); back != testPublicIP {
		t.Errorf("IP round-trip mismatch: got %q", back)
	}
}

func TestIPCounterRoundTrip(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 253, 254, 255, 508, 65023} {
		v4 := fmt.Sprintf("127.0.%d.%d", n/254, n%254+1)
		if got, ok := ipCounterFromFiction(v4); !ok || got != n {
			t.Errorf("v4 %s -> (%d,%v), want %d", v4, got, ok, n)
		}

		if got, ok := ipCounterFromFiction("::ffff:" + v4); !ok || got != n {
			t.Errorf("v6 ::ffff:%s -> (%d,%v), want %d", v4, got, ok, n)
		}
	}
}

// TestOrgNameRequiresWordBoundary is a regression test: a short org name from a
// two-letter domain (ge.com -> "ge") matched as a raw substring and shredded
// unrelated words ("message" -> "messaDEVTARGET").
func TestOrgNameRequiresWordBoundary(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if err := addDomain(engDir, "ge.com", "DEVTARGET"); err != nil {
		t.Fatalf("addDomain: %v", err)
	}

	r2f, _ := rewriters(t, engDir)

	if got := r2f.rewrite("please read the message carefully"); got != "please read the message carefully" {
		t.Errorf("short org name shredded an unrelated word: %q", got)
	}

	if got := r2f.rewrite("the GE incident"); !strings.Contains(got, "DEVTARGET") {
		t.Errorf("org name as a whole word was not rewritten: %q", got)
	}
}

func TestRewriteIsStableOnRepeatedApplication(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)
	r2f, _ := rewriters(t, engDir)

	text := "scan bounty.amazon.com and amazon.com"

	once := r2f.rewrite(text)
	if twice := r2f.rewrite(once); twice != once {
		t.Errorf("rewrite is not idempotent\n  once:  %s\n  twice: %s", once, twice)
	}
}

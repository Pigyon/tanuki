package tanuki

import (
	"slices"
	"strings"
	"testing"
)

// scopeDomain is the fixture target for the scope tests; it is deliberately
// not testBaseDomain, so a scope entry and the shared fixtures cannot be
// confused for one another.
const scopeDomain = "acme.com"

func TestScopeCovers(t *testing.T) {
	t.Parallel()

	sc := parseScope("# a comment\n*.acme.com\n\nsub.other.net\n")

	if !slices.Equal(sc, scope{scopeDomain, "sub.other.net"}) {
		t.Fatalf("parseScope = %v", sc)
	}

	covered := []string{scopeDomain, "www.acme.com", "a.b.acme.com", "sub.other.net", "deep.sub.other.net"}
	for _, d := range covered {
		if !sc.covers(d) {
			t.Errorf("%s is not covered", d)
		}
	}

	for _, d := range []string{"other.net", "notacme.com", "acme.com.evil.io"} {
		if sc.covers(d) {
			t.Errorf("%s is covered", d)
		}
	}
}

// TestEmptyScopeTreatsEverythingAsTarget keeps an engagement that never set a
// scope behaving as it always has.
func TestEmptyScopeTreatsEverythingAsTarget(t *testing.T) {
	t.Parallel()

	if !(scope{}).isTarget("anything.com") {
		t.Error("an empty scope rejected a domain")
	}
}

// TestOutOfScopeDomainGetsNoOrgRule is what scope is for: one docs.python.org
// in tool output used to create an org rule for "python" and rewrite the word
// everywhere, so "written in Python" reached the model as "written in
// DEVTARGET2".
func TestOutOfScopeDomainGetsNoOrgRule(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if _, err := addToScope(engDir, "*.acme.com"); err != nil {
		t.Fatalf("addToScope: %v", err)
	}

	if _, err := mapTargets(engDir, nil, "check bounty.acme.com and docs.python.org"); err != nil {
		t.Fatalf("mapping: %v", err)
	}

	for _, m := range loadMappings(engDir) {
		if m.Type != mapDomain && m.Type != mapWildcard && strings.Contains(m.Real, "python") {
			t.Errorf("out-of-scope domain got a derived mapping: %+v", m)
		}
	}

	r2f, f2r := rewriters(t, engDir)

	const text = "written in Python, see docs.python.org, scan bounty.acme.com"

	got := r2f.rewrite(text)
	if !strings.Contains(got, "written in Python,") {
		t.Errorf("the word Python was rewritten: %q", got)
	}

	for _, host := range []string{"docs.python.org", "bounty.acme.com"} {
		if strings.Contains(got, host) {
			t.Errorf("%s was not hidden: %q", host, got)
		}
	}

	if back := f2r.rewrite(got); back != text {
		t.Errorf("round trip changed the text:\n got %q\nwant %q", back, text)
	}
}

// TestInScopeDomainKeepsDerivedMappings: scope must not cost a real target its
// org, email, path and cloud fictions.
func TestInScopeDomainKeepsDerivedMappings(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if _, err := addToScope(engDir, scopeDomain); err != nil {
		t.Fatalf("addToScope: %v", err)
	}

	if _, err := mapTargets(engDir, nil, "scan bounty.acme.com"); err != nil {
		t.Fatalf("mapping: %v", err)
	}

	for _, want := range []string{mapOrg, mapEmail, mapPath, mapCloud, mapWildcard} {
		if !slices.ContainsFunc(loadMappings(engDir), func(m mappingEntry) bool { return m.Type == want }) {
			t.Errorf("in-scope target has no %s mapping", want)
		}
	}
}

func TestScopeAddIsIdempotentAndRemovable(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	added, err := addToScope(engDir, "ACME.com.")
	if err != nil || !added {
		t.Fatalf("addToScope = %v, %v", added, err)
	}

	if again, _ := addToScope(engDir, "*.acme.com"); again {
		t.Error("the same pattern was added twice")
	}

	removed, err := removeFromScope(engDir, scopeDomain)
	if err != nil || !removed {
		t.Fatalf("removeFromScope = %v, %v", removed, err)
	}

	if len(loadScope(engDir)) != 0 {
		t.Errorf("scope still holds %v", loadScope(engDir))
	}
}

// TestScopeRejectsMalformedPatterns: a stored typo can never match, and since
// any non-empty scope makes everything outside it host-only, a scope of
// nothing but typos quietly downgrades every target.
func TestScopeRejectsMalformedPatterns(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	bad := []string{
		"", "*.", "acme.com\nevil.com", "a|b",
		"*.*.acme.com", "https://acme.com", "acme.com/", "**acme.com", "acme com",
	}
	for _, p := range bad {
		if _, err := addToScope(engDir, p); err == nil {
			t.Errorf("addToScope(%q) was accepted", p)
		}
	}

	if got := loadScope(engDir); len(got) != 0 {
		t.Errorf("a rejected pattern was stored: %v", got)
	}

	for _, p := range []string{scopeDomain, "*.sub.acme.co.uk"} {
		if _, err := addToScope(engDir, p); err != nil {
			t.Errorf("addToScope(%q) was rejected: %v", p, err)
		}
	}
}

// TestInScopeSubdomainOfAnOutOfScopeApex: the apex is mapped first and is not
// in scope, so it was mapped host-only; the subdomain then found the apex
// already mapped and skipped the derived set, leaving the org name, bucket and
// path to go upstream with nothing to refuse them.
func TestInScopeSubdomainOfAnOutOfScopeApex(t *testing.T) {
	engDir := proxyEngagement(t)

	if _, err := addToScope(engDir, "app.acme.com"); err != nil {
		t.Fatalf("addToScope: %v", err)
	}

	out, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"user","content":` +
		`"test https://app.acme.com/ for Acme Corp; bucket s3://acme/dump; path /acme/etc"}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	for _, real := range []string{"Acme", "s3://acme", "/acme/"} {
		if strings.Contains(string(out), real) {
			t.Errorf("%q reached the upstream body: %s", real, out)
		}
	}
}

// TestScopeAddPromotesAnAlreadyMappedDomain: detection never revisits a domain
// it has mapped, so one mapped before its scope entry stayed host-only.
func TestScopeAddPromotesAnAlreadyMappedDomain(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if _, err := addToScope(engDir, "other.example"); err != nil {
		t.Fatalf("addToScope: %v", err)
	}

	if _, err := mapTargets(engDir, nil, "acme corp owns acme.com"); err != nil {
		t.Fatalf("mapTargets: %v", err)
	}

	if slices.ContainsFunc(loadMappings(engDir), func(m mappingEntry) bool { return m.Type == mapOrg }) {
		t.Fatal("an out-of-scope domain got an org mapping")
	}

	if _, err := addToScope(engDir, scopeDomain); err != nil {
		t.Fatalf("addToScope: %v", err)
	}

	if !slices.ContainsFunc(loadMappings(engDir), func(m mappingEntry) bool { return m.Type == mapOrg }) {
		t.Error("scope add did not promote the already-mapped domain")
	}

	r2f, _ := rewriters(t, engDir)
	if got := r2f.rewrite("acme corp"); strings.Contains(got, "acme") {
		t.Errorf("org name still reaches upstream: %q", got)
	}
}

// TestContextDetectedDomainGetsNoOrgRule: recognising any real TLD by its
// surroundings made weak guesses into full targets. ".md", ".py", ".rs" and
// ".sh" are real TLDs, so dev output turned file references into orgs --
// "func main()" became "func DEVTARGET()" -- and one acme.com.community URL
// derived an org for "com", rewriting the word everywhere.
func TestContextDetectedDomainGetsNoOrgRule(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if _, err := mapTargets(engDir, nil,
		"src/main.rs:118 README.md:3 docs.rs/serde https://acme.com.community/ bounty.acme.com"); err != nil {
		t.Fatalf("mapTargets: %v", err)
	}

	for _, m := range loadMappings(engDir) {
		if m.Type == mapDomain || m.Type == mapWildcard {
			continue
		}

		// Only the common-TLD target earns a derived mapping.
		if !strings.Contains(m.Real, "acme") || strings.Contains(m.Real, "community") {
			t.Errorf("a weakly detected domain earned %s|%s|%s", m.Type, m.Real, m.Fiction)
		}
	}

	r2f, _ := rewriters(t, engDir)

	for _, code := range []string{
		"func main() {}", "git merge origin/main", "README section",
		"github.com", "the docs say", "dot com", "import test",
	} {
		if got := r2f.rewrite(code); got != code {
			t.Errorf("rewrite corrupted code: %q -> %q", code, got)
		}
	}
}

// TestScopeStillMapsAnUncommonTLDInFull: the common-TLD rule stands in for a
// scope, so once a scope exists the operator's word is what counts.
func TestScopeStillMapsAnUncommonTLDInFull(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if _, err := addToScope(engDir, "acme.in"); err != nil {
		t.Fatalf("addToScope: %v", err)
	}

	if _, err := mapTargets(engDir, nil, "visit https://portal.acme.in/login"); err != nil {
		t.Fatalf("mapTargets: %v", err)
	}

	if !slices.ContainsFunc(loadMappings(engDir), func(m mappingEntry) bool { return m.Type == mapOrg }) {
		t.Error("an in-scope target on an uncommon TLD got no org mapping")
	}
}

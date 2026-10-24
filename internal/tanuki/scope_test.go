package tanuki

import (
	"slices"
	"strings"
	"testing"
)

func TestScopeCovers(t *testing.T) {
	t.Parallel()

	sc := parseScope("# a comment\n*.acme.com\n\nsub.other.net\n")

	if !slices.Equal(sc, scope{"acme.com", "sub.other.net"}) {
		t.Fatalf("parseScope = %v", sc)
	}

	covered := []string{"acme.com", "www.acme.com", "a.b.acme.com", "sub.other.net", "deep.sub.other.net"}
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

	if _, err := addToScope(engDir, "acme.com"); err != nil {
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

	removed, err := removeFromScope(engDir, "acme.com")
	if err != nil || !removed {
		t.Fatalf("removeFromScope = %v, %v", removed, err)
	}

	if len(loadScope(engDir)) != 0 {
		t.Errorf("scope still holds %v", loadScope(engDir))
	}
}

func TestScopeRejectsValuesThatBreakTheFile(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	for _, bad := range []string{"", "*.", "acme.com\nevil.com", "a|b"} {
		if _, err := addToScope(engDir, bad); err == nil {
			t.Errorf("addToScope(%q) was accepted", bad)
		}
	}
}

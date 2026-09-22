package tanuki

import (
	"path/filepath"
	"strings"
	"testing"
)

// proxyEngagement seeds an engagement and makes it the active one, so the
// proxy helpers that read currentEngagement find it.
func proxyEngagement(t *testing.T, domains ...string) string {
	t.Helper()

	t.Setenv("TANUKI_DATA", t.TempDir())
	initLogger()
	proxyCache.invalidate()

	engDir := engagementDir("guard")
	if err := seedEngagement(engDir, "DEVTARGET"); err != nil {
		t.Fatalf("seeding engagement: %v", err)
	}

	if err := writeFileContent(filepath.Join(dataDir(), "current_engagement"), "guard"); err != nil {
		t.Fatalf("activating engagement: %v", err)
	}

	for _, d := range domains {
		if err := addDomain(engDir, d, ""); err != nil {
			t.Fatalf("addDomain(%q): %v", d, err)
		}
	}

	return engDir
}

// TestProxyMapsTargetsTheHooksNeverSee is the gap this exists to close. The
// hooks only observe the typed prompt and tool output, so a target arriving in
// an @-mentioned file, a paste, or a session resumed elsewhere used to reach
// the API verbatim as long as any other mapping existed.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestProxyMapsTargetsTheHooksNeverSee(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	body := []byte(`{"messages":[{"role":"user","content":"check ` + testBaseDomain +
		` and also secret-target.io at ` + testPublicIP + `"}]}`)

	out, err := anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	for _, real := range []string{testBaseDomain, "secret-target.io", testPublicIP} {
		if strings.Contains(string(out), real) {
			t.Errorf("real value %q reached the upstream body: %s", real, out)
		}
	}
}

// TestProxyMappingsAreStableAcrossRequests: the second request must reuse the
// fictions the first allocated, or the model sees a target change identity
// mid-conversation and the reverse pass breaks.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestProxyMappingsAreStableAcrossRequests(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	body := []byte(`{"content":"scan secret-target.io at ` + testPublicIP + `"}`)

	first, err := anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	second, err := anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("fiction changed between requests:\n  %s\n  %s", first, second)
	}
}

// TestResidualIgnoresFiction: tanuki's own output must not look like a target,
// or the gate would refuse every request it just anonymized correctly.
func TestResidualIgnoresFiction(t *testing.T) {
	t.Parallel()

	fiction := "reach localhost:9000 and localhost:9002//path, mail admin@devtarget.local, " +
		"host 127.0.0.3 and ::ffff:127.0.0.4, bucket file:///tmp/devtarget-prod, ticket DT-42"

	if got := residualTargets(fiction); len(got) > 0 {
		t.Errorf("residualTargets flagged tanuki's own fiction: %v", got)
	}
}

func TestResidualFindsRealValues(t *testing.T) {
	t.Parallel()

	got := residualTargets("ping amazon.com at " + testPublicIP + ", private 10.0.0.1 is fine")

	want := map[string]bool{testBaseDomain: true, testPublicIP: true}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected residual %q", v)
		}

		delete(want, v)
	}

	for v := range want {
		t.Errorf("residualTargets missed %q", v)
	}
}

// TestRefusesWhenAValueSurvives: the gate must block, and the message must
// carry counts only. The client may fold an error into its next prompt, so a
// value named here would leak on the following turn.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestRefusesWhenAValueSurvives(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	err := checkResidual("a stray " + testBaseDomain + " survived")
	if err == nil {
		t.Fatal("checkResidual accepted a body holding a real value")
	}

	if strings.Contains(err.Error(), testBaseDomain) {
		t.Errorf("the refusal names the real value: %v", err)
	}
}

func TestOnLeakWarnForwardsAnyway(t *testing.T) {
	proxyEngagement(t)
	t.Setenv("TANUKI_ON_LEAK", onLeakWarn)

	if err := checkResidual("a stray " + testBaseDomain + " survived"); err != nil {
		t.Errorf("warn policy still refused: %v", err)
	}
}

// TestProxyMapsOnFirstSightInsteadOfRefusing: a fresh engagement used to block
// until a hook had added a mapping. The proxy now allocates one itself, so the
// first request carrying a target is anonymized and forwarded rather than
// refused -- the safety property is that nothing real leaves, not that nothing
// leaves at all.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestProxyMapsOnFirstSightInsteadOfRefusing(t *testing.T) {
	engDir := proxyEngagement(t) // no mappings at all

	out, err := anonymizeUpstreamBody([]byte(`{"content":"scan ` + testBaseDomain + `"}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if strings.Contains(string(out), testBaseDomain) {
		t.Errorf("real domain reached the upstream body: %s", out)
	}

	if !mappingExists(engDir, "domain", testBaseDomain) {
		t.Error("the domain was rewritten but never recorded in mappings.conf")
	}
}

// TestProxyStillRefusesWithNothingMapped keeps the documented behaviour for a
// body that holds no recognisable target: an empty table means the engagement
// is not set up, and a value with an uncurated TLD would slip past detection.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestProxyStillRefusesWithNothingMapped(t *testing.T) {
	proxyEngagement(t)

	if _, err := anonymizeUpstreamBody([]byte(`{"content":"nothing to rewrite"}`)); err == nil {
		t.Error("forwarded a body with no mappings configured")
	}
}

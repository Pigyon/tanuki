package tanuki

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestValidateEngagementNameRejectsTraversal covers the names that reach
// engagementDir from a CLI argument or an import file. engagementDir is a
// plain path join, so anything that escapes the data directory must be
// rejected before it gets there.
func TestValidateEngagementNameRejectsTraversal(t *testing.T) {
	t.Parallel()

	bad := []string{
		"",
		"..",
		"../evil",
		"../../etc/cron.d",
		"a/../../b",
		"sub/dir",
		`win\dir`,
		"..hidden/..",
	}

	for _, name := range bad {
		if err := validateEngagementName(name); err == nil {
			t.Errorf("validateEngagementName(%q) = nil, want error", name)
		}
	}
}

func TestValidateEngagementNameAcceptsPlainNames(t *testing.T) {
	t.Parallel()

	good := []string{"default", "acme-2026", "client_a", "Engagement.1"}

	for _, name := range good {
		if err := validateEngagementName(name); err != nil {
			t.Errorf("validateEngagementName(%q) = %v, want nil", name, err)
		}
	}
}

// TestEngagementDirStaysInDataDir documents why validation is mandatory: the
// path join itself offers no protection.
func TestEngagementDirStaysInDataDir(t *testing.T) {
	t.Setenv("TANUKI_DATA", filepath.Join("var", "lib", "tanuki"))

	escaped := engagementDir("../../../etc")
	if escaped != "etc" {
		t.Fatalf("engagementDir escaped to %q; validation is the only guard", escaped)
	}

	if err := validateEngagementName("../../../etc"); err == nil {
		t.Fatal("validateEngagementName accepted a traversing name")
	}
}

// TestCorruptCounterRefusesAllocation guards against the collision that a
// silent fallback caused: with a corrupt port_counter every new domain
// restarted at the default port, so two real targets shared one fiction.
func TestCorruptCounterRefusesAllocation(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if err := writeFileContent(filepath.Join(engDir, "port_counter"), "not-a-number"); err != nil {
		t.Fatalf("seeding corrupt counter: %v", err)
	}

	if err := addDomain(engDir, "example.com", "DEVTARGET"); err == nil {
		t.Fatal("addDomain succeeded with a corrupt port_counter, want an error")
	}

	if mappingExists(engDir, "domain", "example.com") {
		t.Error("a mapping was written despite the counter failure")
	}
}

// TestMissingCounterUsesDefault keeps the legitimate fallback working: a
// counter file that was never written starts from the default.
func TestMissingCounterUsesDefault(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if err := os.Remove(filepath.Join(engDir, "port_counter")); err != nil {
		t.Fatalf("removing counter: %v", err)
	}

	if err := addDomain(engDir, "example.com", "DEVTARGET"); err != nil {
		t.Fatalf("addDomain: %v", err)
	}

	if !mappingExists(engDir, "domain", "example.com") {
		t.Error("domain was not mapped after falling back to the default counter")
	}
}

// TestDistinctDomainsGetDistinctFictions is the property the counters exist
// to hold: no two real targets may share a fiction value.
func TestDistinctDomainsGetDistinctFictions(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "alpha.com", "beta.com", "gamma.net")

	seen := map[string]string{}

	for _, m := range loadMappings(engDir) {
		if prev, dup := seen[m.Fiction]; dup {
			t.Errorf("fiction %q is shared by %q and %q", m.Fiction, prev, m.Real)
		}

		seen[m.Fiction] = m.Real
	}
}

// TestIPRangeExhaustionFailsClosed: an IP with no fiction counterpart reaches
// the model verbatim, so allocation must fail rather than skip the mapping.
func TestIPRangeExhaustionFailsClosed(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if err := writeFileContent(filepath.Join(engDir, "ip_counter"), strconv.Itoa(maxFictionIPIndex+1)); err != nil {
		t.Fatalf("seeding ip_counter: %v", err)
	}

	if err := addIPMapping(engDir, "8.8.8.8"); err == nil {
		t.Fatal("addIPMapping succeeded past the fiction range, want an error")
	}
}

// TestFictionIPsAreValidAddresses covers the IPv6 branch, which used to skip
// the range guard and emit values like "::ffff:127.0.275.151".
func TestFictionIPsAreValidAddresses(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 253, 254, 255, 64770, maxFictionIPIndex} {
		for _, real := range []string{"8.8.8.8", "2001:db8::1"} {
			fiction, err := fictionIPFor(real, n)
			if err != nil {
				t.Fatalf("fictionIPFor(%q, %d): %v", real, n, err)
			}

			if net.ParseIP(fiction) == nil {
				t.Errorf("fictionIPFor(%q, %d) = %q, not a valid IP", real, n, fiction)
			}

			if got, ok := ipCounterFromFiction(fiction); !ok || got != n {
				t.Errorf("ipCounterFromFiction(%q) = %d, %v; want %d, true", fiction, got, ok, n)
			}
		}
	}
}

// writeRawMapping appends a line to mappings.conf without validation, standing
// in for a hand-edited or legacy table.
func writeRawMapping(t *testing.T, engDir, line string) {
	t.Helper()

	if err := appendToFile(filepath.Join(engDir, "mappings.conf"), line); err != nil {
		t.Fatalf("appending mapping: %v", err)
	}
}

// TestMappingExistsIgnoresMappingContent: the check used to be a substring
// search over the whole file, so a mapping whose *value* looked like a
// mappings.conf line satisfied it and the real domain was never mapped.
func TestMappingExistsIgnoresMappingContent(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	writeRawMapping(t, engDir, "custom|codeword|domain|evil.com|localhost:1")

	if mappingExists(engDir, "domain", "evil.com") {
		t.Error("mappingExists matched inside another mapping's value")
	}

	if !mappingExists(engDir, "custom", "codeword") {
		t.Error("mappingExists missed a real entry")
	}
}

// TestFindNewDomainsIgnoresMappingContent is the leak that follows from it:
// a domain wrongly considered mapped is never given a fiction.
func TestFindNewDomainsIgnoresMappingContent(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	writeRawMapping(t, engDir, "custom|codeword|domain|evil.com|localhost:1")

	found := findNewDomains("please scan evil.com today", engDir)
	if len(found) == 0 {
		t.Fatal("evil.com was treated as already mapped")
	}
}

// TestMappingExistsMatchesWholeFields: a shorter real value must not match a
// longer one that starts with it.
func TestMappingExistsMatchesWholeFields(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t, "amazon.com")

	if mappingExists(engDir, "domain", "amazon.c") {
		t.Error("prefix of a mapped domain reported as mapped")
	}

	if !mappingExists(engDir, "domain", "amazon.com") {
		t.Error("mapped domain reported as missing")
	}
}

// TestConfigureHooksKeepsUnparseableSettings: settings.json was replaced with
// a tanuki-only file whenever it did not parse, so a stray comma cost the
// operator their permissions and env -- silently, on every proxy start.
func TestConfigureHooksKeepsUnparseableSettings(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.MkdirAll(".claude", 0o750); err != nil {
		t.Fatal(err)
	}

	const broken = `{"permissions":{"allow":["Bash(ls:*)"]},}`

	path := filepath.Join(".claude", "settings.json")
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := configureHooks(); err == nil {
		t.Error("configureHooks accepted an unparseable settings.json")
	}

	if got := readFileContent(path); got != broken {
		t.Errorf("settings.json was modified:\n got %s\nwant %s", got, broken)
	}
}

// TestConfigureHooksInstallsOnlyPreToolUse: the proxy covers what the prompt
// and tool-output hooks did, and an upgrade must remove the ones an older
// version installed while keeping the operator's own.
func TestConfigureHooksInstallsOnlyPreToolUse(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.MkdirAll(".claude", 0o750); err != nil {
		t.Fatal(err)
	}

	legacy := `{"hooks":{
		"PostToolUse":[{"hooks":[{"type":"command","command":"docker exec -i tanuki /tanuki hook post-tool-use"}]},
		               {"hooks":[{"type":"command","command":"my-own-linter"}]}],
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"docker exec -i tanuki /tanuki hook user-prompt-submit"}]}]}}`
	if err := os.WriteFile(filepath.Join(".claude", "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := configureHooks(); err != nil {
		t.Fatalf("configureHooks: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	var settings struct {
		Hooks map[string][]any `json:"hooks"`
	}

	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}

	if _, ok := settings.Hooks["UserPromptSubmit"]; ok {
		t.Error("legacy UserPromptSubmit hook was kept")
	}

	if postHooks := settings.Hooks["PostToolUse"]; len(postHooks) != 1 || !strings.Contains(string(raw), "my-own-linter") {
		t.Errorf("PostToolUse = %v, want only the operator's own hook", postHooks)
	}

	if pre := settings.Hooks["PreToolUse"]; len(pre) != 1 {
		t.Errorf("PreToolUse = %v, want the tanuki hook once", pre)
	}

	leftovers, _ := filepath.Glob(filepath.Join(".claude", ".*.tmp"))
	if len(leftovers) > 0 {
		t.Errorf("atomic write left temp files: %v", leftovers)
	}
}

// TestShutdownCleanupReversesConfigure guards the proxy's shutdown path: what
// configureHooks writes on start, removeTanukiSettings must take back on stop.
// Left behind, ANTHROPIC_BASE_URL points the next session at a dead port
// (ECONNREFUSED). The operator's own settings must survive either way.
func TestShutdownCleanupReversesConfigure(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.MkdirAll(".claude", 0o750); err != nil {
		t.Fatal(err)
	}

	ownSettings := `{"permissions":{"allow":["Bash(go test:*)"]},"env":{"MY_VAR":"keep-me"}}`
	settingsPath := filepath.Join(".claude", "settings.json")

	if err := os.WriteFile(settingsPath, []byte(ownSettings), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := configureHooks(); err != nil {
		t.Fatalf("configureHooks: %v", err)
	}

	if raw, _ := os.ReadFile(settingsPath); !strings.Contains(string(raw), "ANTHROPIC_BASE_URL") {
		t.Fatal("configureHooks did not install the proxy redirect")
	}

	removeTanukiSettings(settingsPath)

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.json gone, but it held the operator's own config: %v", err)
	}

	if strings.Contains(string(raw), "ANTHROPIC_BASE_URL") {
		t.Error("redirect survived shutdown; next session would hit a dead port")
	}

	if strings.Contains(string(raw), "tanuki") {
		t.Error("tanuki hook survived shutdown; every tool call would docker exec a gone container")
	}

	if !strings.Contains(string(raw), "keep-me") || !strings.Contains(string(raw), "go test") {
		t.Errorf("operator's own settings were lost: %s", raw)
	}
}

// TestEnvSeedsSubdomains: a subdomain in TANUKI_DOMAINS was skipped whenever
// its apex was already mapped, which the documented example asks for.
func TestEnvSeedsSubdomains(t *testing.T) {
	engDir := proxyEngagement(t)
	t.Setenv("TANUKI_DOMAINS", "Example.COM, api.example.com")

	if err := loadEnvConfig(engDir); err != nil {
		t.Fatalf("loadEnvConfig: %v", err)
	}

	for _, want := range []string{"example.com", "api.example.com"} {
		if !mappingExists(engDir, "domain", want) {
			t.Errorf("%s was not seeded", want)
		}
	}

	// A second start must not re-announce what is already mapped.
	before := loadMappings(engDir)

	if err := loadEnvConfig(engDir); err != nil {
		t.Fatalf("second loadEnvConfig: %v", err)
	}

	if after := loadMappings(engDir); len(after) != len(before) {
		t.Errorf("restart changed the table: %d -> %d mappings", len(before), len(after))
	}
}

// TestMappingValuesCannotCarryTheDelimiter: a "|" in a value shifted the
// fields, so add-rule "a|b" x stored a rule that rewrote "a" to "b|x"; a line
// break let an imported engagement smuggle in whole extra mappings.
func TestMappingValuesCannotCarryTheDelimiter(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	rejected := []mappingEntry{
		{mapCustom, "a|b", "zzz"},
		{mapCustom, "keep", "ok\ncustom|injected|PWNED"},
		{mapCustom, "keep", "ok\rcustom|injected|PWNED"},
		{mapCustom, "", "empty real"},
		{mapCustom, "empty fiction", ""},
		{"ticket", "ACME-", "DT-"},
	}

	for _, m := range rejected {
		if err := addMapping(engDir, m.Type, m.Real, m.Fiction); err == nil {
			t.Errorf("addMapping(%+v) was accepted", m)
		}
	}

	if got := loadMappings(engDir); len(got) != 0 {
		t.Errorf("a rejected mapping was written: %+v", got)
	}

	if err := addMapping(engDir, mapCustom, "Project Phoenix", "Test Alpha"); err != nil {
		t.Errorf("a valid rule was rejected: %v", err)
	}
}

func TestParseArgs(t *testing.T) {
	t.Parallel()

	positional, org, err := parseArgs([]string{"one.com", "two.com", "--fiction-org", "ACME"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}

	if !slices.Equal(positional, []string{"one.com", "two.com"}) {
		t.Errorf("positional = %v, want both domains", positional)
	}

	if org != "ACME" {
		t.Errorf("fictionOrg = %q", org)
	}

	// A mistyped option was skipped in silence, which is how a typo became a
	// target that was never mapped.
	for _, args := range [][]string{{"a.com", "--oops"}, {"a.com", "--fiction-org"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%v) accepted a bad option", args)
		}
	}
}

// TestAddDomainValidatesItsMappings: addDomainMappings built its lines by hand
// and wrote them straight out, so validateMapping never saw them.
// "--fiction-org ACME-CORP" produced org|acme|ACME-CORP, which rewrites "acme"
// into a value that still holds it -- after which the gate refused every body
// mentioning the target, for good. A newline in the fiction org was worse: it
// split the line and left four orphan rows that loadMappings dropped with a
// warning, one of them a four-character pattern reversing to the real domain.
func TestAddDomainValidatesItsMappings(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	for _, fictionOrg := range []string{"ACME-CORP", "DEV\nTARGET", "A|B", "acme-dev"} {
		if err := addDomain(engDir, "acme.com", fictionOrg); err == nil {
			t.Errorf("addDomain accepted --fiction-org %q", fictionOrg)
		}
	}

	if got := loadMappings(engDir); len(got) != 0 {
		t.Errorf("a rejected add wrote %d mappings: %+v", len(got), got)
	}

	if err := addDomain(engDir, "acme.com", "SAFECORP"); err != nil {
		t.Errorf("a sound fiction org was rejected: %v", err)
	}
}

// TestShortOrgInsideItsFictionIsAllowed: the self-containment check was a
// plain substring search, so the org "ge" was refused for sitting inside
// "DEVTARGET" -- where no word-bounded rule can ever match it.
func TestShortOrgInsideItsFictionIsAllowed(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if err := addDomain(engDir, "ge.com", "DEVTARGET"); err != nil {
		t.Errorf("addDomain(ge.com): %v", err)
	}
}

// TestAllocationRefusesWhenItCannotLock: the lock used to log a warning and
// return a no-op release when it could not be taken, so callers allocated
// unserialised -- the duplicate-fiction bug the lock exists to prevent.
//
// Nothing may be written, and the body must still be refused. The refusal now
// comes from the gate rather than from the mapping step, because a mapping
// error names the value and would reach the client.
func TestAllocationRefusesWhenItCannotLock(t *testing.T) {
	engDir := proxyEngagement(t)

	// A lock path that cannot be opened as a file.
	if err := os.MkdirAll(filepath.Join(engDir, "allocate.lock"), 0o750); err != nil {
		t.Fatalf("seeding the blocked lock: %v", err)
	}

	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, _ = mapTargets(engDir, nil, "scan newtarget.com")
		}()
	}

	wg.Wait()

	if got := loadMappings(engDir); len(got) != 0 {
		t.Errorf("an unserialised allocation wrote %d mappings: %+v", len(got), got)
	}

	_, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"user","content":"scan newtarget.com"}]}`))
	if err == nil {
		t.Error("the body was forwarded although the target could not be mapped")
	}

	if err != nil && strings.Contains(err.Error(), "newtarget.com") {
		t.Errorf("the refusal names the real value: %v", err)
	}
}

// TestOrgNamedDevCanBeMapped: the path fiction was "/dev/" + fiction, so an
// org actually called "dev" had a real path its own fiction contained.
// Validation refused the whole entry set, and since Claude Code resends
// history every later request naming that target was refused too, while each
// attempt burned a port and an org number.
func TestOrgNamedDevCanBeMapped(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if err := addDomain(engDir, "dev.io", ""); err != nil {
		t.Fatalf("addDomain(dev.io): %v", err)
	}

	for i := range 3 {
		if _, err := mapTargets(engDir, nil, "scan dev.io now"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	if got := readFileContent(filepath.Join(engDir, "port_counter")); got != "9002" {
		t.Errorf("port_counter drifted to %q, want 9002", got)
	}

	r2f, f2r := rewriters(t, engDir)
	if back := f2r.rewrite(r2f.rewrite("/dev/www")); back != "/dev/www" {
		t.Errorf("path round trip: %q", back)
	}
}

// TestRulesThatWouldRefuseEveryBodyAreRejected: the rewrite takes the longest
// non-overlapping match, so it replaces "AcmeBank" and never looks at the
// "Bank" its own replacement introduced. The gate scans for every match, finds
// it, and refuses the body -- for the rest of the engagement, since the
// history comes back every turn. Each mapping is sound on its own, so checking
// one against itself cannot see it.
func TestRulesThatWouldRefuseEveryBodyAreRejected(t *testing.T) {
	t.Parallel()

	engDir := newTestEngagement(t)

	if _, err := addCustomRule(engDir, "AcmeBank", "DevTarget Bank"); err != nil {
		t.Fatalf("first rule: %v", err)
	}

	if _, err := addCustomRule(engDir, "Bank", "Institution"); err == nil {
		t.Error("a rule that would refuse every body was accepted")
	}

	r2f, _ := rewriters(t, engDir)

	out := r2f.rewrite("the client AcmeBank was tested")
	if got := r2f.survivors(out); len(got) != 0 {
		t.Errorf("the gate would refuse %q over %v", out, got)
	}

	// Unrelated rules are unaffected.
	if _, err := addCustomRule(engDir, "Project Phoenix", "Test Alpha"); err != nil {
		t.Errorf("an unrelated rule was rejected: %v", err)
	}
}

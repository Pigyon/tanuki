package tanuki

import (
	"os"
	"path/filepath"
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

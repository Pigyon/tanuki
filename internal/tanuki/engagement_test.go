package tanuki

import (
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

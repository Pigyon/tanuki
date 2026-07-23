package tanuki

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheReturnsNilForEmptyMappings(t *testing.T) {
	t.Parallel()

	engDir := t.TempDir()
	if err := writeFileContent(filepath.Join(engDir, "mappings.conf"), "# only a comment\n"); err != nil {
		t.Fatalf("seeding mappings: %v", err)
	}

	var c rewriterCache

	rw, err := c.rewriterOrError(engDir, directionR2F)
	if err != nil {
		t.Fatalf("unexpected error for empty mappings: %v", err)
	}

	if rw != nil {
		t.Error("expected nil rewriter for an empty mapping table")
	}
}

// TestCacheReportsReadError is the cache half of the fail-closed guarantee:
// a missing mappings.conf must surface as an error, not a silent empty table.
func TestCacheReportsReadError(t *testing.T) {
	t.Parallel()

	engDir := t.TempDir() // deliberately no mappings.conf

	var c rewriterCache

	if _, err := c.rewriterOrError(engDir, directionR2F); err == nil {
		t.Error("expected a read error for a missing mappings.conf")
	}
}

// TestCacheDoesNotCacheReadError is a regression test: when stat succeeds but
// the read fails (here mappings.conf is a directory), the error must not be
// cached with a non-zero mtime, or the proxy 503s until the mtime changes.
func TestCacheDoesNotCacheReadError(t *testing.T) {
	t.Parallel()

	engDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(engDir, "mappings.conf"), 0o750); err != nil {
		t.Fatalf("creating dir: %v", err)
	}

	var c rewriterCache

	if _, err := c.rewriterOrError(engDir, directionR2F); err == nil {
		t.Fatal("expected a read error when mappings.conf is a directory")
	}

	if c.lastModTime != 0 {
		t.Errorf("read error was cached (lastModTime=%d); it must not be", c.lastModTime)
	}
}

func TestCacheReloadsOnMtimeChange(t *testing.T) {
	t.Parallel()

	engDir := t.TempDir()

	if err := writeFileContent(filepath.Join(engDir, "mappings.conf"), "# start\n"); err != nil {
		t.Fatalf("seeding mappings: %v", err)
	}

	if err := writeFileContent(filepath.Join(engDir, "port_counter"), "9000"); err != nil {
		t.Fatalf("seeding port_counter: %v", err)
	}

	if err := writeFileContent(filepath.Join(engDir, "ip_counter"), "2"); err != nil {
		t.Fatalf("seeding ip_counter: %v", err)
	}

	var c rewriterCache

	if _, err := c.rewriterOrError(engDir, directionR2F); err != nil {
		t.Fatalf("first load: %v", err)
	}

	if err := addDomain(engDir, "example.org", "DEVTARGET"); err != nil {
		t.Fatalf("addDomain: %v", err)
	}

	// Force a distinct mtime so the reload is not missed on coarse-resolution
	// filesystems where two quick writes can share a timestamp.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(engDir, "mappings.conf"), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	rw, err := c.rewriterOrError(engDir, directionR2F)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if rw == nil {
		t.Fatal("expected a rewriter after adding a mapping")
	}

	if got := rw.rewrite("visit example.org"); got == "visit example.org" {
		t.Error("cache did not reload after mappings.conf changed")
	}
}

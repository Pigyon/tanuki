package tanuki

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with stdout redirected and returns what it printed, so
// a verify report does not scribble over the test output.
func captureStdout(t *testing.T, fn func() int) (out string, code int) {
	t.Helper()

	old := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	os.Stdout = w

	t.Cleanup(func() { os.Stdout = old })

	done := make(chan string, 1)

	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()

	code = fn()

	_ = w.Close()

	return <-done, code
}

//nolint:paralleltest // captureStdout swaps a process-wide file handle.
func TestVerifyMappingsPassesOnASoundTable(t *testing.T) {
	engDir := newTestEngagement(t, testBaseDomain, testSubDomain)

	out, code := captureStdout(t, func() int {
		return verifyMappings(loadMappings(engDir))
	})

	if code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, out)
	}

	if !strings.Contains(out, "0 leaks") {
		t.Errorf("report did not confirm a clean run:\n%s", out)
	}
}

// TestVerifyMappingsCatchesANoOpMapping: a mapping whose fiction equals its
// real value rewrites to itself and protects nothing. verify exists to find
// exactly this kind of quiet misconfiguration.
//
//nolint:paralleltest // captureStdout swaps a process-wide file handle.
func TestVerifyMappingsCatchesANoOpMapping(t *testing.T) {
	engDir := newTestEngagement(t, testBaseDomain)

	if err := addMapping(engDir, "custom", "prod-secret", "prod-secret"); err != nil {
		t.Fatalf("addMapping: %v", err)
	}

	out, code := captureStdout(t, func() int {
		return verifyMappings(loadMappings(engDir))
	})

	if code == 0 {
		t.Errorf("exit = 0, want non-zero\n%s", out)
	}

	if !strings.Contains(out, "prod-secret") {
		t.Errorf("report did not name the offending mapping:\n%s", out)
	}
}

// TestVerifyInputFindsOnlyUnmappedValues is the check worth running over a
// draft report: mapped targets are covered and must not be reported, unmapped
// ones must be.
//
//nolint:paralleltest // captureStdout swaps a process-wide file handle.
func TestVerifyInputFindsOnlyUnmappedValues(t *testing.T) {
	engDir := newTestEngagement(t, testBaseDomain)

	if err := addIPMapping(engDir, testPublicIP); err != nil {
		t.Fatalf("addIPMapping: %v", err)
	}

	report := filepath.Join(t.TempDir(), "report.md")

	content := "Tested " + testSubDomain + " at " + testPublicIP + ".\n" +
		"Also partner-portal.acme-supplier.io (93.184.216.34).\n"
	if err := os.WriteFile(report, []byte(content), 0o600); err != nil {
		t.Fatalf("writing report: %v", err)
	}

	rw := newRewriter(loadMappings(engDir), directionR2F)

	out, code := captureStdout(t, func() int {
		return verifyInput([]string{report}, rw)
	})

	if code == 0 {
		t.Errorf("exit = 0 despite unmapped values\n%s", out)
	}

	for _, want := range []string{"partner-portal.acme-supplier.io", "93.184.216.34"} {
		if !strings.Contains(out, want) {
			t.Errorf("unmapped value %q not reported:\n%s", want, out)
		}
	}

	for _, covered := range []string{testSubDomain, testPublicIP} {
		if strings.Contains(out, covered) {
			t.Errorf("mapped value %q reported as a leak:\n%s", covered, out)
		}
	}
}

//nolint:paralleltest // captureStdout swaps a process-wide file handle.
func TestVerifyInputAcceptsACleanReport(t *testing.T) {
	engDir := newTestEngagement(t, testBaseDomain)

	report := filepath.Join(t.TempDir(), "clean.md")
	if err := os.WriteFile(report, []byte("Tested "+testSubDomain+" thoroughly.\n"), 0o600); err != nil {
		t.Fatalf("writing report: %v", err)
	}

	rw := newRewriter(loadMappings(engDir), directionR2F)

	out, code := captureStdout(t, func() int {
		return verifyInput([]string{report}, rw)
	})

	if code != 0 {
		t.Errorf("exit = %d on a fully mapped report, want 0\n%s", code, out)
	}
}

// TestRedirectedStdinIgnoresAPipe is why verify cannot hang: a pipe nobody
// writes to would block io.ReadAll forever, and "docker run -i" supplies
// exactly that. Pipe input has to be asked for with "-".
//
//nolint:paralleltest // swaps os.Stdin.
func TestRedirectedStdinIgnoresAPipe(t *testing.T) {
	old := os.Stdin

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	defer func() {
		os.Stdin = old

		_ = r.Close()
		_ = w.Close()
	}()

	os.Stdin = r

	if redirectedStdin() {
		t.Error("a pipe was treated as redirected input; verify would block on it")
	}
}

//nolint:paralleltest // swaps os.Stdin.
func TestRedirectedStdinAcceptsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening file: %v", err)
	}

	old := os.Stdin
	os.Stdin = f

	defer func() {
		os.Stdin = old

		_ = f.Close()
	}()

	if !redirectedStdin() {
		t.Error(`"tanuki verify < file" was not recognised`)
	}
}

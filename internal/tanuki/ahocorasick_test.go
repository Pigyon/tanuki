package tanuki

import (
	"strings"
	"testing"
)

func TestACReplaceAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		patterns     []string
		replacements []string
		input        string
		expected     string
	}{
		{
			name:         "single match",
			patterns:     []string{testBaseDomain},
			replacements: []string{testFictionBase},
			input:        "go to amazon.com now",
			expected:     "go to localhost:9000 now",
		},
		{
			name:         "longest of two overlapping patterns wins",
			patterns:     []string{testSubDomain, testBaseDomain},
			replacements: []string{testFictionWildcard, testFictionBase},
			input:        testSubDomain,
			expected:     testFictionWildcard,
		},
		{
			name:         "longest wins regardless of pattern order",
			patterns:     []string{testBaseDomain, testSubDomain},
			replacements: []string{testFictionBase, testFictionWildcard},
			input:        testSubDomain,
			expected:     testFictionWildcard,
		},
		{
			name:         "repeated occurrences all replaced",
			patterns:     []string{"acme"},
			replacements: []string{"dev"},
			// Repetition is the point: every occurrence must be replaced.
			input:    "acme acme acme", //nolint:dupword // intentional repetition
			expected: "dev dev dev",    //nolint:dupword // intentional repetition
		},
		{
			name:         "no match is a passthrough",
			patterns:     []string{"acme"},
			replacements: []string{"dev"},
			input:        "nothing to do here",
			expected:     "nothing to do here",
		},
		{
			name:         "adjacent matches",
			patterns:     []string{"ab", "cd"},
			replacements: []string{"1", "2"},
			input:        "abcd",
			expected:     "12",
		},
		{
			name:         "match at start and end",
			patterns:     []string{"x"},
			replacements: []string{"y"},
			input:        "x middle x",
			expected:     "y middle y",
		},
		{
			name:         "empty text",
			patterns:     []string{"a"},
			replacements: []string{"b"},
			input:        "",
			expected:     "",
		},
		{
			name:         "replacement containing the pattern does not loop",
			patterns:     []string{"a"},
			replacements: []string{"aa"},
			input:        "a",
			expected:     "aa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newACMachine(tt.patterns, tt.replacements, nil)
			if got := m.replaceAll(tt.input); got != tt.expected {
				t.Errorf("replaceAll(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// TestACOverlapSelection pins the selection rule for overlapping matches:
// earliest start wins, and at the same start the longest pattern wins.
// Text left over after the selected match is passed through untouched
// rather than being rescanned, so "aaaa" yields "X" + the trailing "a".
func TestACOverlapSelection(t *testing.T) {
	t.Parallel()

	m := newACMachine([]string{"aaa", "aa"}, []string{"X", "Y"}, nil)

	if got, want := m.replaceAll("aaaa"), "Xa"; got != want {
		t.Errorf("replaceAll(%q) = %q, want %q", "aaaa", got, want)
	}

	if got, want := m.replaceAll("aa"), "Y"; got != want {
		t.Errorf("replaceAll(%q) = %q, want %q", "aa", got, want)
	}
}

func TestACEmptyMachine(t *testing.T) {
	t.Parallel()

	m := newACMachine(nil, nil, nil)
	if got := m.replaceAll("untouched"); got != "untouched" {
		t.Errorf("empty machine modified text: %q", got)
	}
}

func TestACBinarySafety(t *testing.T) {
	t.Parallel()

	// The machine is byte-based; multi-byte UTF-8 must pass through intact
	// when it is not part of a pattern.
	m := newACMachine([]string{"target"}, []string{"project"}, nil)

	text := "スキャン target のホスト"

	got := m.replaceAll(text)
	if !strings.Contains(got, "スキャン") || !strings.Contains(got, "のホスト") {
		t.Errorf("multi-byte text was corrupted: %q", got)
	}

	if strings.Contains(got, "target") {
		t.Errorf("pattern not replaced: %q", got)
	}
}

func BenchmarkACReplaceAll(b *testing.B) {
	patterns := []string{
		testBaseDomain, testSubDomain, "admin@amazon.com",
		testPublicIP, "s3://amazon-prod", "/var/www/amazon/",
	}
	replacements := []string{
		testFictionBase, testFictionWildcard, "admin@devtarget.local",
		"127.0.0.2", "file:///tmp/devtarget-prod", "/dev/project/",
	}

	m := newACMachine(patterns, replacements, nil)
	text := strings.Repeat("scan bounty.amazon.com from 52.94.236.248 and mail admin@amazon.com. ", 100)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = m.replaceAll(text)
	}
}

func TestDomainContinues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		i    int
		want bool
	}{
		{"end of text", "acme.com", 8, false},
		{"longer label", "acme.community", 8, true},
		{"longer name", "acme.com.br", 8, true},
		{"trailing dot", "acme.com.", 8, false},
		{"sentence end", "visit acme.com. next", 14, false},
		{"path", "acme.com/x", 8, false},
		{"port", "acme.com:443", 8, false},
		{"comma", "acme.com, x", 8, false},
		{"hyphen then label", "acme.com-b", 8, true},
		{"hyphen then space", "acme.com- ", 8, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := domainContinues(tt.text, tt.i); got != tt.want {
				t.Errorf("domainContinues(%q, %d) = %v, want %v", tt.text, tt.i, got, tt.want)
			}
		})
	}
}

// TestBoundedPatternsSkipLongerHostnames is the corruption this guards
// against: "amazon.com" firing inside "amazon.community" replaced half of an
// unrelated name and produced "localhost:9000munity".
func TestBoundedPatternsSkipLongerHostnames(t *testing.T) {
	t.Parallel()

	m := newACMachine(
		[]string{testBaseDomain, "s3://amazon"},
		[]string{testFictionBase, "file:///tmp/devtarget"},
		[]bool{true, false},
	)

	tests := []struct{ in, want string }{
		{"amazon.community", "amazon.community"},
		{"amazon.com.br", "amazon.com.br"},
		{"amazon.com", testFictionBase},
		{"amazon.com.", testFictionBase + "."},
		{"amazon.com/x", testFictionBase + "/x"},
		// Unbounded patterns stay greedy on purpose: a bucket prefix must
		// keep covering every bucket that starts with it.
		{"s3://amazon-logs", "file:///tmp/devtarget-logs"},
	}

	for _, tt := range tests {
		if got := m.replaceAll(tt.in); got != tt.want {
			t.Errorf("replaceAll(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

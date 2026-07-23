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
			patterns:     []string{"amazon.com"},
			replacements: []string{"localhost:9000"},
			input:        "go to amazon.com now",
			expected:     "go to localhost:9000 now",
		},
		{
			name:         "longest of two overlapping patterns wins",
			patterns:     []string{"bounty.amazon.com", "amazon.com"},
			replacements: []string{"localhost:9001", "localhost:9000"},
			input:        "bounty.amazon.com",
			expected:     "localhost:9001",
		},
		{
			name:         "longest wins regardless of pattern order",
			patterns:     []string{"amazon.com", "bounty.amazon.com"},
			replacements: []string{"localhost:9000", "localhost:9001"},
			input:        "bounty.amazon.com",
			expected:     "localhost:9001",
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

			m := newACMachine(tt.patterns, tt.replacements)
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

	m := newACMachine([]string{"aaa", "aa"}, []string{"X", "Y"})

	if got, want := m.replaceAll("aaaa"), "Xa"; got != want {
		t.Errorf("replaceAll(%q) = %q, want %q", "aaaa", got, want)
	}

	if got, want := m.replaceAll("aa"), "Y"; got != want {
		t.Errorf("replaceAll(%q) = %q, want %q", "aa", got, want)
	}
}

func TestACEmptyMachine(t *testing.T) {
	t.Parallel()

	m := newACMachine(nil, nil)
	if got := m.replaceAll("untouched"); got != "untouched" {
		t.Errorf("empty machine modified text: %q", got)
	}
}

func TestACBinarySafety(t *testing.T) {
	t.Parallel()

	// The machine is byte-based; multi-byte UTF-8 must pass through intact
	// when it is not part of a pattern.
	m := newACMachine([]string{"target"}, []string{"project"})

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
		"amazon.com", "bounty.amazon.com", "admin@amazon.com",
		"52.94.236.248", "s3://amazon-prod", "/var/www/amazon/",
	}
	replacements := []string{
		"localhost:9000", "localhost:9001", "admin@devtarget.local",
		"127.0.0.2", "file:///tmp/devtarget-prod", "/dev/project/",
	}

	m := newACMachine(patterns, replacements)
	text := strings.Repeat("scan bounty.amazon.com from 52.94.236.248 and mail admin@amazon.com. ", 100)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = m.replaceAll(text)
	}
}

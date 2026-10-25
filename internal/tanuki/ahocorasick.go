package tanuki

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fold lowercases an ASCII byte for case-insensitive matching, so a mixed-case
// target ("aMaZoN") is not missed and leaked. Matches slice the original text.
func fold(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}

	return b
}

type acNode struct {
	children map[byte]*acNode
	fail     *acNode
	output   int
	depth    int
}

// boundKind says what has to surround a match for it to count.
type boundKind uint8

const (
	// boundNone matches anywhere. A cloud prefix or custom rule is meant to
	// cover every longer string that starts with it.
	boundNone boundKind = iota
	// boundHost refuses a match that is only the start of a longer hostname.
	boundHost
	// boundWord needs a non-word character or the edge of the text on both
	// sides, as the regexp \b does.
	boundWord
	// boundAddress keeps a mapped IP from matching inside a longer address.
	boundAddress
	// boundWordPart also accepts the edges of one part of a compound
	// identifier: "acme_prod", "AcmeCloud", "getACMEToken".
	boundWordPart
)

type acMachine struct {
	root     *acNode
	patterns []string
	replace  []string
	// bounds says, per pattern, where a match may start and end; see
	// keepBounded. A nil slice bounds nothing.
	bounds []boundKind
}

func newACMachine(patterns, replacements []string, bounds []boundKind) *acMachine {
	m := &acMachine{
		root:     &acNode{children: map[byte]*acNode{}},
		patterns: patterns,
		replace:  replacements,
		bounds:   bounds,
	}

	for i, p := range patterns {
		m.insert(p, i)
	}

	m.buildFailLinks()

	return m
}

func (m *acMachine) insert(pattern string, index int) {
	node := m.root

	for i := range len(pattern) {
		b := fold(pattern[i])

		child, ok := node.children[b]
		if !ok {
			child = &acNode{children: map[byte]*acNode{}, output: -1, depth: i + 1}
			node.children[b] = child
		}

		node = child
	}

	node.output = index
}

func (m *acMachine) buildFailLinks() {
	queue := []*acNode{}
	m.root.fail = m.root

	for _, child := range m.root.children {
		child.fail = m.root
		queue = append(queue, child)
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for b, child := range current.children {
			queue = append(queue, child)

			fail := current.fail

			for fail != m.root {
				if _, ok := fail.children[b]; ok {
					break
				}

				fail = fail.fail
			}

			if next, ok := fail.children[b]; ok && next != child {
				child.fail = next
			} else {
				child.fail = m.root
			}
		}
	}
}

type acMatch struct {
	start int
	end   int
	index int
}

func (m *acMachine) search(text string) []acMatch {
	matches := []acMatch{}
	node := m.root

	for i := range len(text) {
		b := fold(text[i])

		for node != m.root {
			if _, ok := node.children[b]; ok {
				break
			}

			node = node.fail
		}

		if child, ok := node.children[b]; ok {
			node = child
		}

		temp := node

		for temp != m.root {
			if temp.output >= 0 {
				patLen := len(m.patterns[temp.output])
				matches = append(matches, acMatch{
					start: i - patLen + 1,
					end:   i + 1,
					index: temp.output,
				})
			}

			temp = temp.fail
		}
	}

	return matches
}

// find returns every match whose bound holds, overlapping ones included.
func (m *acMachine) find(text string) []acMatch {
	return m.keepBounded(text, m.search(text))
}

func (m *acMachine) replaceAll(text string) string {
	if len(m.patterns) == 0 {
		return text
	}

	matches := m.find(text)
	if len(matches) == 0 {
		return text
	}

	selected := selectLongestNonOverlapping(matches, m.patterns)
	if len(selected) == 0 {
		return text
	}

	var result []byte

	pos := 0

	for _, match := range selected {
		result = append(result, text[pos:match.start]...)
		result = append(result, m.replace[match.index]...)
		pos = match.end
	}

	result = append(result, text[pos:]...)

	return string(result)
}

// isLabelByte reports whether b can appear inside a hostname label.
func isLabelByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-', b == '_':
		return true
	default:
		return false
	}
}

// domainContinues reports whether the hostname matched up to i runs on into a
// longer name, making the match a prefix of a different host.
//
// A dot decides nothing on its own: "acme.com.br" and "acme.community" are
// other hosts, while "acme.com.txt" is a file named after one. The whole token
// is what carries on -- and only when it is itself a domain.
func domainContinues(text string, start, i int) bool {
	if i >= len(text) {
		return false
	}

	b := text[i]

	switch {
	case b == '-':
		// A separator continues the name only when a label follows it, so
		// "acme.com-" at the end of a word is still the whole host.
		return i+1 < len(text) && isLabelByte(text[i+1])
	case b != '.':
		return isLabelByte(b)
	case i+1 >= len(text) || !isLabelByte(text[i+1]):
		// "acme.com." at the end of a sentence is still the whole host.
		return false
	}

	return isRealDomain(hostToken(text, start, i))
}

// hostToken returns the whole hostname-shaped run starting at start.
func hostToken(text string, start, i int) string {
	end := i

	for end < len(text) && (isLabelByte(text[end]) || text[end] == '.') {
		end++
	}

	// A trailing dot is punctuation, not a label.
	for end > i && text[end-1] == '.' {
		end--
	}

	return strings.ToLower(text[start:end])
}

// addressContinues reports whether the byte at i could carry on an IP
// address, so a match ending (or starting) there is part of a longer one.
func addressContinues(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}

	b := text[i]

	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'f', b >= 'A' && b <= 'F':
		return true
	default:
		return b == '.' || b == ':'
	}
}

// keepBounded drops matches their pattern's bound rules out. It runs before
// the longest match is chosen, so a longer pattern that fails its bound leaves
// the way clear for a shorter one that holds.
func (m *acMachine) keepBounded(text string, matches []acMatch) []acMatch {
	if m.bounds == nil {
		return matches
	}

	kept := make([]acMatch, 0, len(matches))

	for _, match := range matches {
		if m.isBounded(text, match) {
			kept = append(kept, match)
		}
	}

	return kept
}

// isBounded checks a host only on the right: refusing a match *preceded* by
// label bytes would leave "notacme.com" whole, and hiding the target matters
// more than leaving an unrelated name intact. The org rule still covers the
// bare label in whatever the automaton declines.
func (m *acMachine) isBounded(text string, match acMatch) bool {
	if match.index >= len(m.bounds) {
		return true
	}

	switch m.bounds[match.index] {
	case boundHost:
		return !domainContinues(text, match.start, match.end)
	case boundWord:
		return wordEdge(text, match.start) && wordEdge(text, match.end)
	case boundAddress:
		return !addressContinues(text, match.start-1) && !addressContinues(text, match.end)
	case boundWordPart:
		return partEdge(text, match.start) && partEdge(text, match.end)
	case boundNone:
		return true
	}

	return true
}

// around returns the runes before and at i, and the one after that;
// utf8.RuneError stands in for any that falls outside text.
func around(text string, i int) (before, at, after rune) {
	before, at, after = utf8.RuneError, utf8.RuneError, utf8.RuneError

	if i > 0 {
		before, _ = utf8.DecodeLastRuneInString(text[:i])
	}

	if i < len(text) {
		var size int

		at, size = utf8.DecodeRuneInString(text[i:])

		if i+size < len(text) {
			after, _ = utf8.DecodeRuneInString(text[i+size:])
		}
	}

	return before, at, after
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordEdge reports whether i is a word boundary, counting "_" as part of a
// word as \b does, but for every script rather than ASCII alone.
func wordEdge(text string, i int) bool {
	before, at, _ := around(text, i)

	isWord := func(r rune) bool { return isAlnum(r) || r == '_' }

	return !isWord(before) || !isWord(at)
}

// partEdge reports whether i starts or ends one part of an identifier: a
// separator ("_" included) or the edge of the text, a digit giving way to a
// letter, a lower-case letter to an upper-case one ("acme|Cloud"), or the
// last capital of an upper-case run that starts a new word ("ACME|Token").
//
// A letter giving way to a digit is deliberately not an edge. Fiction orgs
// are numbered (DEVTARGET, DEVTARGET2), so rewriting "acme" inside "acme2"
// would produce another org's fiction and reverse to the wrong target.
func partEdge(text string, i int) bool {
	before, at, after := around(text, i)

	switch {
	case !isAlnum(before) || !isAlnum(at):
		return true
	case unicode.IsDigit(before) && unicode.IsLetter(at):
		return true
	case unicode.IsLower(before) && unicode.IsUpper(at):
		return true
	default:
		return unicode.IsUpper(before) && unicode.IsUpper(at) && unicode.IsLower(after)
	}
}

func selectLongestNonOverlapping(matches []acMatch, patterns []string) []acMatch {
	if len(matches) == 0 {
		return nil
	}

	sorted := make([]acMatch, len(matches))
	copy(sorted, matches)

	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}

		return len(patterns[sorted[i].index]) > len(patterns[sorted[j].index])
	})

	selected := make([]acMatch, 0, len(sorted))
	lastEnd := 0

	for _, m := range sorted {
		if m.start >= lastEnd {
			selected = append(selected, m)
			lastEnd = m.end
		}
	}

	return selected
}

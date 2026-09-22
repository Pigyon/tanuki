package tanuki

import "sort"

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

type acMachine struct {
	root     *acNode
	patterns []string
	replace  []string
	// bounded marks patterns that are hostnames and so must not match inside
	// a longer name; see dropContinuedMatches. A nil slice bounds nothing.
	bounded []bool
}

func newACMachine(patterns, replacements []string, bounded []bool) *acMachine {
	m := &acMachine{
		root:     &acNode{children: map[byte]*acNode{}},
		patterns: patterns,
		replace:  replacements,
		bounded:  bounded,
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

func (m *acMachine) replaceAll(text string) string {
	if len(m.patterns) == 0 {
		return text
	}

	matches := m.dropContinuedMatches(text, m.search(text))
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

// domainContinues reports whether the text at i carries on a hostname, so the
// match ending there is only a prefix of a longer name.
func domainContinues(text string, i int) bool {
	if i >= len(text) {
		return false
	}

	if b := text[i]; b == '.' || b == '-' {
		// A separator continues the name only when a label follows it, so
		// "amazon.com." at the end of a sentence is still the whole host.
		return i+1 < len(text) && isLabelByte(text[i+1])
	}

	return isLabelByte(text[i])
}

// dropContinuedMatches discards a bounded pattern that matched only the start
// of a longer hostname, so the mapping for "acme.com" does not fire inside
// "acme.community" or "acme.com.br" and rewrite half of a different name.
//
// Only the right-hand side is checked. Refusing a match that is *preceded* by
// label bytes would leave "notacme.com" whole, and hiding the target matters
// more than leaving an unrelated name intact; the org rule still covers the
// bare label in whatever the automaton declines.
func (m *acMachine) dropContinuedMatches(text string, matches []acMatch) []acMatch {
	if m.bounded == nil {
		return matches
	}

	kept := make([]acMatch, 0, len(matches))

	for _, match := range matches {
		if match.index < len(m.bounded) && m.bounded[match.index] && domainContinues(text, match.end) {
			continue
		}

		kept = append(kept, match)
	}

	return kept
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

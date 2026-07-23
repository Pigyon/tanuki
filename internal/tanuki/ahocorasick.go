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
}

func newACMachine(patterns, replacements []string) *acMachine {
	m := &acMachine{
		root:     &acNode{children: map[byte]*acNode{}},
		patterns: patterns,
		replace:  replacements,
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

	matches := m.search(text)
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

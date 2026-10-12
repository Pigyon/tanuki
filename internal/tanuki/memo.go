package tanuki

import "sync"

const (
	// memoMinLen is the shortest string worth remembering: below it, scanning
	// costs less than the lookup.
	memoMinLen = 256
	// memoMaxBytes bounds each cache. Key and value are about the same size,
	// so an entry counts twice its key.
	memoMaxBytes = 64 << 20
)

// memoCache remembers what a pure function returned for long strings. Claude
// Code resends the whole conversation on every turn, so nearly every string in
// a request was in the previous one; recalling the result instead of scanning
// again makes that history almost free. A full cache is cleared outright:
// simpler than eviction, and it costs one turn of rescanning.
type memoCache[V any] struct {
	mu    sync.Mutex
	items map[string]V
	size  int
}

func (c *memoCache[V]) get(key string, compute func(string) V) V {
	if len(key) < memoMinLen {
		return compute(key)
	}

	c.mu.Lock()
	v, ok := c.items[key]
	c.mu.Unlock()

	if ok {
		return v
	}

	v = compute(key)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.items == nil || c.size+2*len(key) > memoMaxBytes {
		c.items = map[string]V{}
		c.size = 0
	}

	c.items[key] = v
	c.size += 2 * len(key)

	return v
}

package tanuki

import (
	"os"
	"path/filepath"
	"sync"
)

type rewriterCache struct {
	mu          sync.RWMutex
	engDir      string
	mappings    []mappingEntry
	r2f         *rewriter
	f2r         *rewriter
	lastModTime int64
}

var proxyCache rewriterCache

func (c *rewriterCache) getRewriter(engDir, direction string) *rewriter {
	mappingsPath := filepath.Join(engDir, "mappings.conf")
	currentMod := fileMtime(mappingsPath)

	c.mu.RLock()
	if c.valid(engDir, currentMod) {
		defer c.mu.RUnlock()
		return c.pick(direction)
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	currentMod = fileMtime(mappingsPath)
	if c.valid(engDir, currentMod) {
		return c.pick(direction)
	}

	c.engDir = engDir
	c.mappings = loadMappings(engDir)

	if len(c.mappings) > 0 {
		c.r2f = newRewriter(c.mappings, "r2f")
		c.f2r = newRewriter(c.mappings, "f2r")
	} else {
		c.r2f = nil
		c.f2r = nil
	}

	c.lastModTime = currentMod

	return c.pick(direction)
}

// valid reports whether the cache holds rewriters for engDir at the given
// mappings.conf modification time. Callers must hold c.mu.
func (c *rewriterCache) valid(engDir string, mod int64) bool {
	return c.engDir == engDir && mod == c.lastModTime && c.lastModTime != 0
}

// pick returns the cached rewriter for the given direction. Callers must hold c.mu.
func (c *rewriterCache) pick(direction string) *rewriter {
	if direction == "r2f" {
		return c.r2f
	}

	return c.f2r
}

func fileMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}

	return info.ModTime().UnixNano()
}

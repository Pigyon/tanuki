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
	loadErr     error
	lastModTime int64
}

var proxyCache rewriterCache

// getRewriter returns the cached rewriter (nil if empty/unreadable). f2r uses
// this; r2f must use rewriterOrError so a read failure fails closed.
func (c *rewriterCache) getRewriter(engDir, direction string) *rewriter {
	rw, _ := c.rewriterOrError(engDir, direction)
	return rw
}

// rewriterOrError returns the cached rewriter and any read error. A nil
// rewriter with a nil error means the file was read but holds no mappings.
func (c *rewriterCache) rewriterOrError(engDir, direction string) (*rewriter, error) {
	mappingsPath := filepath.Join(engDir, "mappings.conf")
	currentMod := fileMtime(mappingsPath)

	c.mu.RLock()

	if c.isValid(engDir, currentMod) {
		defer c.mu.RUnlock()

		return c.pick(direction), c.loadErr
	}

	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	currentMod = fileMtime(mappingsPath)
	if c.isValid(engDir, currentMod) {
		return c.pick(direction), c.loadErr
	}

	c.engDir = engDir
	c.mappings, c.loadErr = readMappingsFile(engDir)

	if len(c.mappings) > 0 {
		c.r2f = newRewriter(c.mappings, directionR2F)
		c.f2r = newRewriter(c.mappings, directionF2R)
	} else {
		c.r2f = nil
		c.f2r = nil
	}

	// Never cache a read error: stat can succeed while the read fails, and a
	// non-zero mtime would pin the error until mappings.conf's mtime changes.
	if c.loadErr != nil {
		c.lastModTime = 0
	} else {
		c.lastModTime = currentMod
	}

	return c.pick(direction), c.loadErr
}

// isValid reports whether the cache holds rewriters for engDir at the given
// mappings.conf modification time. Callers must hold c.mu.
func (c *rewriterCache) isValid(engDir string, mod int64) bool {
	return c.engDir == engDir && mod == c.lastModTime && c.lastModTime != 0
}

// pick returns the cached rewriter for the given direction. Callers must hold c.mu.
func (c *rewriterCache) pick(direction string) *rewriter {
	if direction == directionR2F {
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

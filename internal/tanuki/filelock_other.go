//go:build !unix

package tanuki

import (
	"os"
	"sync"
)

// processLocks serialises holders within this process, keyed by lock path.
var processLocks sync.Map

// lockFile serialises within the process only. tanuki ships as a Linux
// container, where the unix build takes an advisory lock the kernel releases
// on crash; this keeps the package building elsewhere without pretending to
// coordinate between processes.
func lockFile(f *os.File) (unlock func() error, err error) {
	mu, _ := processLocks.LoadOrStore(f.Name(), &sync.Mutex{})

	mutex := mu.(*sync.Mutex)
	mutex.Lock()

	return func() error {
		mutex.Unlock()
		return nil
	}, nil
}

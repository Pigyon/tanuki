package tanuki

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

var processLocks sync.Map

func acquireFileLock(path string) func() {
	lockPath := path + ".lock"
	lockDir := filepath.Dir(lockPath)

	_ = os.MkdirAll(lockDir, 0755)

	mu, _ := processLocks.LoadOrStore(lockPath, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()

	for range 100 {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			f.Close()

			return func() {
				os.Remove(lockPath)
				mu.(*sync.Mutex).Unlock()
			}
		}

		if info, statErr := os.Stat(lockPath); statErr == nil {
			if time.Since(info.ModTime()) > 10*time.Second {
				os.Remove(lockPath)

				continue
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	if logger != nil {
		logger.Warn("file lock acquisition failed, proceeding without lock", "path", lockPath)
	}

	return func() {
		mu.(*sync.Mutex).Unlock()
	}
}

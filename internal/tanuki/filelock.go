package tanuki

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	lockAttempts    = 100
	lockRetryDelay  = 50 * time.Millisecond
	lockStaleAfter  = 10 * time.Second
	lockDirMode     = 0o750
	lockFileModeExc = 0o600
)

var processLocks sync.Map

// warnErr logs err as a warning unless it is nil or a benign "already gone"
// result. Lock bookkeeping should never abort the caller's operation.
func warnErr(err error, msg, path string) {
	isNothingToReport := err == nil || os.IsNotExist(err)
	if isNothingToReport || logger == nil {
		return
	}

	logger.Warn(msg, "path", path, "error", err)
}

// tryLockFile attempts to create the exclusive lock file. It reports whether
// the lock was taken.
func tryLockFile(lockPath string) bool {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, lockFileModeExc)
	if err != nil {
		return false
	}

	warnErr(f.Close(), "closing lock file failed", lockPath)

	return true
}

// reclaimIfStale removes a lock file left behind by a crashed process and
// reports whether it did, so the caller can retry without waiting.
func reclaimIfStale(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil || time.Since(info.ModTime()) <= lockStaleAfter {
		return false
	}

	warnErr(os.Remove(lockPath), "removing stale lock failed", lockPath)

	return true
}

// acquireFileLock serialises writes to path across goroutines (in-process
// mutex) and processes (O_EXCL lock file). The returned release is safe to call.
func acquireFileLock(path string) func() {
	lockPath := path + ".lock"

	warnErr(os.MkdirAll(filepath.Dir(lockPath), lockDirMode), "creating lock directory failed", lockPath)

	mu, _ := processLocks.LoadOrStore(lockPath, &sync.Mutex{})

	mutex, ok := mu.(*sync.Mutex)
	if !ok {
		return func() {}
	}

	mutex.Lock()

	for range lockAttempts {
		if tryLockFile(lockPath) {
			return func() {
				warnErr(os.Remove(lockPath), "removing lock file failed", lockPath)
				mutex.Unlock()
			}
		}

		if reclaimIfStale(lockPath) {
			continue
		}

		time.Sleep(lockRetryDelay)
	}

	if logger != nil {
		logger.Warn("file lock acquisition failed, proceeding without lock", "path", lockPath)
	}

	return mutex.Unlock
}

package tanuki

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	lockDirMode  = 0o750
	lockFileMode = 0o600
)

// warnErr logs err as a warning unless it is nil or a benign "already gone"
// result. Lock bookkeeping should never abort the caller's operation.
func warnErr(err error, msg, path string) {
	isNothingToReport := err == nil || os.IsNotExist(err)
	if isNothingToReport || logger == nil {
		return
	}

	logger.Warn(msg, "path", path, "error", err)
}

// acquireFileLock serialises writes to path across goroutines and processes,
// and returns the release. Checking that a value has no mapping and appending
// one has to be a single step: run apart, concurrent requests naming the same
// new target each gave it a different fiction.
//
// A lock that cannot be taken is an error, never a warning to carry on past.
// Degrading to a no-op let callers allocate unserialised, which is the bug the
// lock exists to prevent.
//
// The lock file is created once and then left in place. Removing it on release
// is what lets a second holder's lock be deleted by the first.
func acquireFileLock(path string) (release func(), err error) {
	lockPath := path + ".lock"

	if err := os.MkdirAll(filepath.Dir(lockPath), lockDirMode); err != nil {
		return nil, fmt.Errorf("creating lock directory for %s: %w", lockPath, err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}

	unlock, err := lockFile(f)
	if err != nil {
		warnErr(f.Close(), "closing lock file failed", lockPath)

		return nil, fmt.Errorf("locking %s: %w", lockPath, err)
	}

	return func() {
		warnErr(unlock(), "unlocking file failed", lockPath)
		warnErr(f.Close(), "closing lock file failed", lockPath)
	}, nil
}

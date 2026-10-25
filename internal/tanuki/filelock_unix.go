//go:build unix

package tanuki

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock on f, waiting for any other holder
// to release it, and returns the unlock.
//
// The kernel drops the lock when the process dies, so a crash cannot leave one
// behind. That is what the previous O_EXCL lock file could not do: it needed a
// staleness timeout to guess whether a holder was still alive, gave up and
// carried on unlocked after five seconds, and let the reclaiming process
// delete a lock another had since taken.
//
// A lock belongs to the open file description, so two goroutines that each
// open the file contend exactly as two processes do.
func lockFile(f *os.File) (unlock func() error, err error) {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}

	return func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
}

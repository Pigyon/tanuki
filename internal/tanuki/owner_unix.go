//go:build unix

package tanuki

import (
	"os"
	"strconv"
	"syscall"
)

// hostOwner returns who files in dir should belong to: the directory's owner,
// or TANUKI_UID/TANUKI_GID when the directory is root-owned, which is what
// Docker creates for a bind mount whose host path did not exist. ok is false
// when there is no one but root to give them to.
func hostOwner(dir string) (uid, gid int, ok bool) {
	if info, err := os.Stat(dir); err == nil {
		if st, isStat := info.Sys().(*syscall.Stat_t); isStat && st.Uid != 0 {
			return int(st.Uid), int(st.Gid), true
		}
	}

	uid, uidErr := strconv.Atoi(os.Getenv("TANUKI_UID"))
	gid, gidErr := strconv.Atoi(os.Getenv("TANUKI_GID"))

	if uidErr != nil || gidErr != nil || uid <= 0 || gid < 0 {
		return 0, 0, false
	}

	return uid, gid, true
}

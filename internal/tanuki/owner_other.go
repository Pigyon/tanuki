//go:build !unix

package tanuki

// hostOwner has no file ownership to report on this platform.
func hostOwner(string) (uid, gid int, ok bool) {
	return 0, 0, false
}

//go:build unix

package store

import "syscall"

// diskFreeBytes reports the free bytes on dir's filesystem via statfs(2).
// It reads Bavail (not Bfree) because that's what's actually available to
// an unprivileged process — some filesystems reserve a slice of Bfree for
// root — matching what a write from lion's own (unprivileged) process could
// really use, which is the quantity asDatabaseFull needs to compare against
// the --max-db-size cap's remaining headroom.
func diskFreeBytes(dir string) (int64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, false
	}
	return int64(stat.Bavail) * int64(stat.Bsize), true
}

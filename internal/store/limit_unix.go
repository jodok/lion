//go:build linux || darwin

package store

import "syscall"

// diskFreeBytes reports the free bytes on dir's filesystem via statfs(2).
//
// Deliberately linux+darwin rather than the broad `unix` tag: Statfs_t's
// field names differ across the BSDs (OpenBSD spells these F_bavail/F_bsize,
// NetBSD ships a deprecated zero-sized struct), so the broad tag breaks
// compilation on unix targets this file never actually supported. lion ships
// linux and darwin binaries; everywhere else reports "unknown", which
// asDatabaseFull already treats as "preserve the original error".
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

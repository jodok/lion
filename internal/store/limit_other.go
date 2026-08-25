//go:build !linux && !darwin

package store

// diskFreeBytes has no portable equivalent of statfs(2) on this platform, so
// it always reports "unknown" — asDatabaseFull treats that the same as any
// other case where the disk-vs-cap comparison can't be substantiated:
// preserve the original error rather than guess which one caused it.
func diskFreeBytes(dir string) (int64, bool) {
	return 0, false
}

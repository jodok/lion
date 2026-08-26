//go:build unix

package store

import "os"

// dirWritableByOthers reports whether dir's mode lets users other than its
// owner create entries in it — the property that makes the store's
// check-then-open sequence losable to a symlink swap (see ensureStoreDir).
func dirWritableByOthers(fi os.FileInfo) bool {
	return fi.Mode().Perm()&0o022 != 0
}

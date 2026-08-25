//go:build !unix

package store

import "os"

// dirWritableByOthers always reports false here: Go synthesizes unix-style
// permission bits on Windows (an ordinary writable directory reads as 0777),
// so the unix group/other-write test would reject every default store path
// on an OS whose actual access control lives in ACLs this check can't see.
// Refusing nothing beats refusing everything; the symlink refusal and
// descriptor-based chmod in ensureFileMode still apply everywhere.
func dirWritableByOthers(fi os.FileInfo) bool {
	return false
}

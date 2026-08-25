//go:build !unix

package ratelimit

import "errors"

// lockSupported is false here: without flock there is no way to serialize
// writers across processes, so NewDefault avoids the persisted limiter
// entirely instead of leaving every call to fail closed on a lock it can
// never take.
const lockSupported = false

// lockFile has no implementation on non-unix platforms. It must still fail
// rather than silently return a no-op unlock: the persisted budget's
// cross-process safety depends on this lock actually serializing writers, so
// a platform without flock has to fail closed (via ErrBudgetLock in Wait)
// rather than reserve unlocked.
func lockFile(lockPath string) (unlock func(), err error) {
	return nil, errors.New("ratelimit: inter-process state locking is not supported on this platform")
}

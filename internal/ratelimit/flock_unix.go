//go:build unix

package ratelimit

import (
	"os"
	"syscall"
)

// lockFile blocks until it holds an exclusive flock on lockPath, creating the
// file if it doesn't exist. Unlike a PID- or mtime-based lock file, an flock
// is released by the kernel the instant the holding process exits, crashes,
// or is killed — so a process that dies mid-reservation leaves no lock for a
// waiter to misjudge as either "held" or "stale". That's what lets Wait block
// indefinitely here instead of giving up after a bounded wait: there is never
// a legitimate reason to proceed without the lock, because the lock can never
// outlive the process that holds it.
func lockFile(lockPath string) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// LOCK_EX blocks until acquired. The critical section it guards (load,
	// check budget, reserve, save) is microseconds of local file I/O, so
	// blocking here is safe and preferable to a bounded wait that can time
	// out while the lock is still legitimately held by a live process.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

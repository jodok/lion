//go:build unix

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// supported is true here: unix flock is a real inter-process lock.
const supported = true

// openLockFile opens path for locking, refusing to follow a symlink at that
// location. A lock path derives from a configurable, attacker-influenceable
// directory (lion's --store flag, for the sync lock), so pre-placing a
// symlink there and letting flock/WriteInfo operate through it would let
// another user with write access to that directory redirect either
// operation onto a file they don't own — WriteInfo would then overwrite
// that file with lock-holder JSON. O_NOFOLLOW makes the open itself fail
// (ELOOP) the instant the final path component is a symlink, which is part
// of the same open(2) call and therefore immune to the check-then-open race
// a standalone Lstat would leave open; the explicit Lstat below exists only
// to turn that failure into a clear error message instead of a bare errno.
func openLockFile(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("lockfile: %s is a symlink; refusing to use it as a lock file", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("lockfile: %s is a symlink; refusing to use it as a lock file", path)
		}
		return nil, err
	}
	return f, nil
}

func acquire(path string) (release func(), err error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	// LOCK_EX blocks until acquired. See Acquire's doc comment for why
	// blocking indefinitely is correct for this call.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func tryAcquire(path string) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	// LOCK_NB makes Flock return EWOULDBLOCK immediately instead of waiting,
	// which is what lets TryAcquire distinguish "busy" (ErrLocked, worth
	// retrying or reporting) from a genuine I/O error.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrLocked
		}
		return nil, err
	}
	release := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
	return &Lock{f: f, release: release}, nil
}

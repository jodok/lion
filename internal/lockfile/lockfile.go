// Package lockfile provides a cross-process advisory lock backed by the
// kernel's flock, plus a tiny best-effort "who's holding this" info channel
// built on top of the same lock path.
//
// It exists so internal/ratelimit and internal/store don't each carry their
// own build-tagged flock implementation. flock is what makes a lock file
// left behind by a killed process safe to reuse instantly: the kernel
// releases it the moment the holding process exits, crashes, or is killed,
// so there is never a stale-lock file for a second acquirer to misjudge as
// either "held" or "abandoned" — unlike a PID- or mtime-based lock file.
package lockfile

import (
	"errors"
	"os"
)

// ErrLocked is returned by TryAcquire when another process already holds
// the lock. It is a distinct sentinel (rather than a generic error) so a
// caller that wants to fail fast, retry, or report the holder can tell
// "busy" apart from a real I/O error (e.g. an unwritable directory).
var ErrLocked = errors.New("lockfile: held by another process")

// Acquire blocks until it holds an exclusive lock on path, creating the file
// if it doesn't exist, and returns a func that releases it.
//
// This is for lion's own short, microsecond-scale critical sections (see
// internal/ratelimit.Wait) where blocking forever is the correct behavior:
// the critical section is load-check-reserve-save of a small local file, so
// there is never a legitimate reason to give up rather than wait, and the
// lock can never outlive the process that holds it.
func Acquire(path string) (release func(), err error) {
	return acquire(path)
}

// TryAcquire attempts the same lock without blocking. If another process
// already holds it, it returns ErrLocked immediately instead of waiting, so
// a caller that wants to fail fast — or poll on its own timeout, reporting
// progress in between — can do so. This is what `lion sync`'s store lock
// uses instead of Acquire: an interactive command failing fast (or waiting a
// bounded, user-chosen amount via --lock-wait) is far more useful than one
// that blocks indefinitely with no feedback.
//
// It returns a *Lock rather than a bare release func because the store
// lock's holder-info payload (see Lock.WriteInfo) must be written through
// the exact descriptor opened here — path derives from the configurable
// --store flag, so re-opening it by name to write that payload would let
// anyone able to swap a symlink in after acquisition redirect the write.
func TryAcquire(path string) (*Lock, error) {
	return tryAcquire(path)
}

// Lock is a held lock returned by TryAcquire. It keeps the descriptor that
// was opened (and, on unix, flock'd) at acquire time so WriteInfo can
// publish through that same descriptor instead of reopening path by name.
type Lock struct {
	f       *os.File
	release func()
}

// Release releases the lock and closes its descriptor.
func (l *Lock) Release() {
	l.release()
}

// WriteInfo overwrites the lock file's content with b, through the
// descriptor obtained when the lock was acquired — never by reopening the
// path by name, which would race a symlink swapped in after acquisition
// (see TryAcquire's doc comment).
func (l *Lock) WriteInfo(b []byte) error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.WriteAt(b, 0); err != nil {
		return err
	}
	return nil
}

// Supported reports whether this platform has a real inter-process lock.
// Used by callers that need to decide between a locked, persisted mode and
// an unlocked fallback rather than failing every call closed on a platform
// where the lock can never be taken.
func Supported() bool {
	return supported
}

// ReadInfo best-effort reads whatever the current holder (if any) wrote via
// Lock.WriteInfo. It never blocks and never takes the lock itself — it's a
// diagnostic peek for a human-readable message, not a synchronization
// primitive — so the content it returns can be empty, stale, or torn by a
// concurrent WriteInfo. A missing file or any read error simply yields no
// info, which callers should treat as "unknown holder" rather than propagate.
//
// Unlike WriteInfo, this genuinely has no held descriptor to write through —
// it runs in a process that lost the race for the lock, possibly before the
// winner has created the file at all — so it can only reopen path by name.
// The Lstat guard below still refuses to follow a symlink there: reading
// through one can't corrupt anything, but it could hang forever on a FIFO an
// attacker planted at the lock path, which would turn a "best-effort
// diagnostic" into a hang.
func ReadInfo(path string) []byte {
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

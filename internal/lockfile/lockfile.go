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
func TryAcquire(path string) (release func(), err error) {
	return tryAcquire(path)
}

// Supported reports whether this platform has a real inter-process lock.
// Used by callers that need to decide between a locked, persisted mode and
// an unlocked fallback rather than failing every call closed on a platform
// where the lock can never be taken.
func Supported() bool {
	return supported
}

// WriteInfo overwrites path's content with b. It is meant to be called only
// by the current lock holder, immediately after Acquire/TryAcquire
// succeeds, so a concurrent acquirer that loses the race can read back who
// is holding the lock (see ReadInfo) instead of seeing a bare "locked".
//
// It does not itself take the lock — the caller already holds it — and a
// failure here is the caller's to handle; this package doesn't decide
// whether an unwritable info payload should block proceeding, since the
// lock itself (not the info) is what provides correctness.
func WriteInfo(path string, b []byte) error {
	// O_TRUNC here is safe precisely because the caller already holds the
	// flock: nothing else can be reading path in a way that a torn write
	// would corrupt for a concurrent *holder* (there can only be one), and a
	// concurrent *waiter* calling ReadInfo is documented as best-effort/racy.
	return os.WriteFile(path, b, 0o600)
}

// ReadInfo best-effort reads whatever the current holder (if any) wrote via
// WriteInfo. It never blocks and never takes the lock itself — it's a
// diagnostic peek for a human-readable message, not a synchronization
// primitive — so the content it returns can be empty, stale, or torn by a
// concurrent WriteInfo. A missing file or any read error simply yields no
// info, which callers should treat as "unknown holder" rather than propagate.
func ReadInfo(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

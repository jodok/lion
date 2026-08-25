package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jodok/lion/internal/lockfile"
)

// lockHolder is written into LockPath by whoever currently holds it, so a
// losing acquirer can report who that is (wacli's convention: "another sync
// is running (pid 1234, started 12:03:11)") instead of a bare "locked" a
// user has nothing to act on — and, in particular, nothing that tells them
// whether it's safe to `rm` a lock file that looks stale.
type lockHolder struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

// lockPollInterval is how often Lock retries TryAcquire while waiting out
// --lock-wait. It's a plain poll rather than a blocking wait because the
// underlying primitive (lockfile.TryAcquire) is non-blocking by design —
// see that package's doc comment — and polling is what lets Lock still
// notice ctx cancellation while waiting.
const lockPollInterval = 250 * time.Millisecond

// LockSupported reports whether this platform has a real inter-process
// lock (see internal/lockfile.Supported). `lion sync` uses this to decide
// whether to warn on stderr that concurrent-run protection isn't available
// before calling Lock, which silently no-ops rather than failing on such a
// platform (see Lock).
func (s *Store) LockSupported() bool {
	return lockfile.Supported()
}

// Lock acquires the exclusive sync lock at LockPath, so two concurrent
// `lion sync` runs can't interleave writes. Readers (export, search) must
// never call this — only a writer needs exclusivity, and blocking a read
// on a long-running sync would defeat the point of keeping the network
// pass and the read pass separate.
//
// It fails fast by default (waitFor == 0). Pass waitFor > 0 (lion sync's
// --lock-wait) to instead poll up to that long before giving up, so a
// script that expects a brief overlap between two scheduled syncs doesn't
// have to fail on the first attempt.
//
// On a platform with no inter-process lock (see LockSupported), Lock
// degrades to an unlocked no-op rather than making sync refuse to run at
// all — the same trade-off internal/ratelimit.NewDefault makes for its own
// persisted state lock: a weaker guarantee is the lesser evil versus a CLI
// that cannot run. lion ships unix binaries, where the locked path is
// always the one taken.
func (s *Store) Lock(ctx context.Context, waitFor time.Duration) (release func(), err error) {
	if !s.LockSupported() {
		return func() {}, nil
	}

	deadline := time.Now().Add(waitFor)
	for {
		release, err := lockfile.TryAcquire(s.LockPath())
		if err == nil {
			// Best-effort: a failure to record who's holding the lock must
			// not fail the lock itself, since the lock (not the info) is
			// what provides correctness. A losing acquirer just falls back
			// to a less specific message (see holderSuffix).
			if info, mErr := json.Marshal(lockHolder{PID: os.Getpid(), Started: time.Now()}); mErr == nil {
				_ = lockfile.WriteInfo(s.LockPath(), info)
			}
			return release, nil
		}
		if err != lockfile.ErrLocked {
			return nil, fmt.Errorf("store: acquire sync lock: %w", err)
		}
		if waitFor <= 0 || !time.Now().Before(deadline) {
			return nil, fmt.Errorf("another lion sync is running%s; pass --lock-wait to wait for it instead of failing immediately", holderSuffix(s.LockPath()))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// holderSuffix formats whatever the current lock holder recorded (see
// Lock), for a clear error message. It returns "" — silently degrading the
// message rather than erroring — when there's nothing to report: the info
// file predates this feature, was written by a process that raced the
// write and lost, or simply isn't there yet.
func holderSuffix(lockPath string) string {
	b := lockfile.ReadInfo(lockPath)
	if len(b) == 0 {
		return ""
	}
	var h lockHolder
	if err := json.Unmarshal(b, &h); err != nil || h.PID == 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d, started %s)", h.PID, h.Started.Local().Format("15:04:05"))
}

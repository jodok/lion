package ratelimit

import "github.com/jodok/lion/internal/lockfile"

// lockFile and lockSupported are thin adapters over internal/lockfile,
// kept as package-level names (rather than calling lockfile.Acquire /
// lockfile.Supported directly at each call site) so this refactor is a pure
// move: Wait's call sites and every existing test in limiter_test.go
// (TestLockFileIsExclusive, TestWaitFailsClosedWhenLockUnavailable, ...)
// keep working unchanged. The actual flock implementation now lives in
// internal/lockfile, shared with internal/store's sync lock — see that
// package's doc comment for why.
var (
	lockFile      = lockfile.Acquire
	lockSupported = lockfile.Supported()
)

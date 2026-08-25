package ratelimit

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestLimiter returns a limiter whose clock and sleep are controlled so tests
// don't actually wait. sleep is a no-op; now advances by the reserved gap on
// each Wait via the returned advance function is unnecessary because Wait
// reserves against wall-clock-free logic — we just verify reservation math and
// budget enforcement.
func newTestLimiter(budgets map[Class]Budget) *Limiter {
	l := New(budgets)
	l.sleep = func(context.Context, time.Duration) error { return nil }
	return l
}

func TestDailyBudgetEnforced(t *testing.T) {
	l := newTestLimiter(map[Class]Budget{
		Invite: {MinGap: 0, MaxGap: 0, DailyMax: 3},
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx, Invite); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	if err := l.Wait(ctx, Invite); err != ErrDailyBudget {
		t.Fatalf("4th invite: got %v, want ErrDailyBudget", err)
	}
}

func TestDailyBudgetWindowExpiry(t *testing.T) {
	l := newTestLimiter(map[Class]Budget{
		Write: {MinGap: 0, MaxGap: 0, DailyMax: 1},
	})
	// Freeze "now" so we can move it deterministically.
	base := time.Now()
	l.now = func() time.Time { return base }
	ctx := context.Background()
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(ctx, Write); err != ErrDailyBudget {
		t.Fatalf("got %v, want ErrDailyBudget", err)
	}
	// Advance past the rolling 24h window; the one action recorded above
	// ages out and the budget frees up, regardless of any calendar boundary.
	l.now = func() time.Time { return base.Add(25 * time.Hour) }
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatalf("after window expiry: %v", err)
	}
}

// TestWaitConcurrent exercises the limiter from many goroutines to catch data
// races under `go test -race` (rnd and shared maps must be protected).
func TestWaitConcurrent(t *testing.T) {
	l := newTestLimiter(map[Class]Budget{
		Read: {MinGap: time.Millisecond, MaxGap: 2 * time.Millisecond, DailyMax: 0},
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Wait(ctx, Read)
		}()
	}
	wg.Wait()
}

func TestUnknownClassIsNoop(t *testing.T) {
	l := newTestLimiter(map[Class]Budget{})
	if err := l.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("unknown class should be no-op, got %v", err)
	}
}

// F14(a): the very first action of a class must still incur the configured
// jitter/min-gap. Before the fix, a class with no prior nextAt computed a
// zero wait, so a freshly started process could fire its first mutating
// action instantly.
func TestFirstActionAppliesGap(t *testing.T) {
	l := New(map[Class]Budget{
		Write: {MinGap: 3 * time.Second, MaxGap: 3 * time.Second, DailyMax: 0},
	})
	var slept time.Duration
	l.sleep = func(_ context.Context, d time.Duration) error {
		slept = d
		return nil
	}
	if err := l.Wait(context.Background(), Write); err != nil {
		t.Fatal(err)
	}
	if slept < 3*time.Second {
		t.Errorf("first action slept %v, want >= 3s (MinGap)", slept)
	}
}

// F14(b): a second Limiter constructed over the same state file (simulating
// a second `lion invite` process spawned by a shell loop) must see the
// first process's consumed daily budget rather than starting fresh.
func TestPersistedBudgetSurvivesNewLimiter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimit.json")
	budgets := map[Class]Budget{Invite: {MinGap: 0, MaxGap: 0, DailyMax: 1}}

	l1 := NewPersistent(budgets, path)
	l1.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l1.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("first process: %v", err)
	}

	// A fresh Limiter over the same path stands in for a second process.
	l2 := NewPersistent(budgets, path)
	l2.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l2.Wait(context.Background(), Invite); err != ErrDailyBudget {
		t.Fatalf("second process: got %v, want ErrDailyBudget (budget should carry over)", err)
	}
}

// F14(b) / rolling-window regression: a calendar-date reset let a caller
// spend the whole invite/write budget right before local midnight and the
// whole budget again right after — double the intended burst, and a process
// in a different timezone could reset the shared file repeatedly. This test
// picks times either side of midnight that are only 2h apart to prove the
// budget does NOT reset there: it must stay spent until a full 24h has
// elapsed since the action was recorded, not until the calendar date changes.
func TestPersistedBudgetIsRollingNotCalendarDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimit.json")
	budgets := map[Class]Budget{Invite: {MinGap: 0, MaxGap: 0, DailyMax: 1}}
	beforeMidnight := time.Date(2026, 8, 24, 23, 0, 0, 0, time.UTC)
	afterMidnight := beforeMidnight.Add(2 * time.Hour) // 2026-08-25 01:00 UTC, new calendar day

	l1 := NewPersistent(budgets, path)
	l1.now = func() time.Time { return beforeMidnight }
	l1.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l1.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("first invite: %v", err)
	}

	// Crossing midnight 2h later must NOT free the budget: a calendar-day
	// reset here is exactly the double-spend this defect reintroduced.
	l2 := NewPersistent(budgets, path)
	l2.now = func() time.Time { return afterMidnight }
	l2.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l2.Wait(context.Background(), Invite); err != ErrDailyBudget {
		t.Fatalf("2h later, across midnight: got %v, want ErrDailyBudget (rolling window, not calendar day)", err)
	}
}

// TestRollingWindowAges verifies the two boundary cases explicitly: an
// action 23h old is still inside the rolling 24h window and must still
// count against the budget; the same action 25h old has aged out and must
// not.
func TestRollingWindowAges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimit.json")
	budgets := map[Class]Budget{Invite: {MinGap: 0, MaxGap: 0, DailyMax: 1}}
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	l1 := NewPersistent(budgets, path)
	l1.now = func() time.Time { return base }
	l1.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l1.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("first invite: %v", err)
	}

	l2 := NewPersistent(budgets, path)
	l2.now = func() time.Time { return base.Add(23 * time.Hour) }
	l2.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l2.Wait(context.Background(), Invite); err != ErrDailyBudget {
		t.Fatalf("23h later: got %v, want ErrDailyBudget (still inside the rolling window)", err)
	}

	l3 := NewPersistent(budgets, path)
	l3.now = func() time.Time { return base.Add(25 * time.Hour) }
	l3.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l3.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("25h later: budget should have freed up, got %v", err)
	}
}

// TestRollingWindowFreesExactlyExpiredSlots checks that expiry is per-action
// rather than a bulk reset: with a multi-slot budget, staggered reservations
// must age out of the window one at a time as it slides, not all at once the
// way a calendar-day counter would.
func TestRollingWindowFreesExactlyExpiredSlots(t *testing.T) {
	l := newTestLimiter(map[Class]Budget{
		Write: {MinGap: 0, MaxGap: 0, DailyMax: 2},
	})
	base := time.Now()
	ctx := context.Background()

	l.now = func() time.Time { return base }
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatalf("action 1: %v", err)
	}

	l.now = func() time.Time { return base.Add(time.Hour) }
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatalf("action 2: %v", err)
	}

	// Both slots are now spent.
	l.now = func() time.Time { return base.Add(2 * time.Hour) }
	if err := l.Wait(ctx, Write); err != ErrDailyBudget {
		t.Fatalf("got %v, want ErrDailyBudget", err)
	}

	// 24h after action 1 (23h after action 2): only action 1 has aged out,
	// freeing exactly one slot for a new reservation.
	l.now = func() time.Time { return base.Add(24 * time.Hour) }
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatalf("after first slot expires: %v", err)
	}

	// The freed slot is immediately spent again (action 2 from t=1h and the
	// new one from t=24h both still count), so a third reservation must fail.
	l.now = func() time.Time { return base.Add(24*time.Hour + 30*time.Minute) }
	if err := l.Wait(ctx, Write); err != ErrDailyBudget {
		t.Fatalf("got %v, want ErrDailyBudget", err)
	}
}

// TestConcurrentLimitersDoNotLoseReservations simulates several `lion`
// processes running at once: each gets its own Limiter over one shared state
// file. Without an inter-process lock they all read the same counter and the
// last writer wins, so the daily budget silently over-issues — the shell-loop
// bypass that persisting the budget is meant to stop.
func TestConcurrentLimitersDoNotLoseReservations(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "ratelimit.json")

	const procs = 8
	budgets := map[Class]Budget{Write: {DailyMax: 100, MinGap: 0, MaxGap: 0}}

	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A distinct Limiter per goroutine stands in for a distinct
			// process: no shared in-memory mutex, only the file lock.
			l := NewPersistent(budgets, statePath)
			if err := l.Wait(context.Background(), Write); err != nil {
				t.Errorf("Wait: %v", err)
			}
		}()
	}
	wg.Wait()

	final := NewPersistent(budgets, statePath)
	if got := len(final.actions[Write]); got != procs {
		t.Fatalf("persisted write count = %d, want %d (reservations were lost to a cross-process race)", got, procs)
	}
}

// TestLockFileIsExclusive verifies flock actually blocks a second acquirer
// until the first releases. This is what makes a lock file left behind by a
// killed process safe to reuse instantly, with no staleness heuristic
// needed: the kernel releases the flock the moment the holding process dies,
// so a live holder always blocks a second acquirer and a dead one never does.
func TestLockFileIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimit.json.lock")

	unlock1, err := lockFile(path)
	if err != nil {
		t.Fatalf("first lockFile: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		unlock2, err := lockFile(path)
		if err != nil {
			t.Errorf("second lockFile: %v", err)
			return
		}
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("second lockFile acquired the lock while the first still held it")
	case <-time.After(200 * time.Millisecond):
		// Expected: the second acquirer is still blocked.
	}

	unlock1()

	select {
	case <-acquired:
		// Expected: releasing the first lock let the second proceed.
	case <-time.After(2 * time.Second):
		t.Fatal("second lockFile never acquired the lock after the first released it")
	}
}

// TestWaitFailsClosedWhenLockUnavailable ensures that when the inter-process
// lock cannot be acquired, Wait returns an error instead of silently
// reserving unlocked. Falling back to unlocked reservation is exactly the
// cross-process bypass persistence exists to prevent (see
// TestConcurrentLimitersDoNotLoseReservations), so an unavailable lock must
// be a hard stop.
func TestWaitFailsClosedWhenLockUnavailable(t *testing.T) {
	// statePath sits under a parent directory that doesn't exist, so the
	// ".lock" sibling file can never be created and lockFile always fails.
	statePath := filepath.Join(t.TempDir(), "missing-parent", "ratelimit.json")
	budgets := map[Class]Budget{Write: {MinGap: 0, MaxGap: 0, DailyMax: 100}}

	l := NewPersistent(budgets, statePath)
	l.sleep = func(context.Context, time.Duration) error { return nil }

	err := l.Wait(context.Background(), Write)
	if !errors.Is(err, ErrBudgetLock) {
		t.Fatalf("got %v, want an error wrapping ErrBudgetLock", err)
	}
	if len(l.actions[Write]) != 0 {
		t.Fatalf("reservation was made despite failing to acquire the lock: %v", l.actions[Write])
	}
}

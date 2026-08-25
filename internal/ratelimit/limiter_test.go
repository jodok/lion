package ratelimit

import (
	"context"
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
	// Advance past midnight; the daily counter should reset.
	l.now = func() time.Time { return base.Add(25 * time.Hour) }
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatalf("after day rollover: %v", err)
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

// F14(b): a persisted budget resets when the stored date is no longer
// today, mirroring TestDailyBudgetWindowExpiry but across the process
// boundary a state file introduces.
func TestPersistedBudgetResetsOnDayRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimit.json")
	budgets := map[Class]Budget{Invite: {MinGap: 0, MaxGap: 0, DailyMax: 1}}
	day1 := time.Date(2026, 8, 24, 23, 0, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Hour) // 2026-08-25 01:00 UTC

	l1 := NewPersistent(budgets, path)
	l1.now = func() time.Time { return day1 }
	l1.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l1.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("day 1: %v", err)
	}
	if err := l1.Wait(context.Background(), Invite); err != ErrDailyBudget {
		t.Fatalf("day 1, second invite: got %v, want ErrDailyBudget", err)
	}

	l2 := NewPersistent(budgets, path)
	l2.now = func() time.Time { return day2 }
	l2.sleep = func(context.Context, time.Duration) error { return nil }
	if err := l2.Wait(context.Background(), Invite); err != nil {
		t.Fatalf("day 2: budget should have reset across the rollover, got %v", err)
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
	if got := final.counts[Write]; got != procs {
		t.Fatalf("persisted write count = %d, want %d (reservations were lost to a cross-process race)", got, procs)
	}
}

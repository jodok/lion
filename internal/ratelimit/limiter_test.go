package ratelimit

import (
	"context"
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
	// Advance beyond the 24h window; the old action should be pruned.
	l.now = func() time.Time { return base.Add(25 * time.Hour) }
	if err := l.Wait(ctx, Write); err != nil {
		t.Fatalf("after window: %v", err)
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

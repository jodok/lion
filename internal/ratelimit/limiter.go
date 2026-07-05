// Package ratelimit provides conservative, human-like pacing for LinkedIn
// actions. LinkedIn aggressively restricts accounts that behave like bots,
// so lion paces requests with randomized inter-action gaps and enforces
// per-day action budgets.
package ratelimit

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"
)

// ErrDailyBudget is returned when a class's rolling-24h action budget is spent.
var ErrDailyBudget = errors.New("daily action budget exhausted for this class")

// Class groups actions by how sensitive LinkedIn is to them. Reads are cheap;
// invites are the most likely to trigger restrictions.
type Class int

const (
	Read Class = iota
	Write
	Invite
)

// Budget describes pacing for one action class.
type Budget struct {
	// MinGap and MaxGap bound the randomized delay between actions.
	MinGap, MaxGap time.Duration
	// DailyMax caps actions per rolling 24h (0 = unlimited).
	DailyMax int
}

// DefaultBudgets are intentionally cautious, tuned for a throwaway account.
// They can be overridden via config.
func DefaultBudgets() map[Class]Budget {
	return map[Class]Budget{
		Read:   {MinGap: 800 * time.Millisecond, MaxGap: 2500 * time.Millisecond, DailyMax: 0},
		Write:  {MinGap: 3 * time.Second, MaxGap: 9 * time.Second, DailyMax: 60},
		Invite: {MinGap: 20 * time.Second, MaxGap: 60 * time.Second, DailyMax: 20},
	}
}

// Limiter paces actions and enforces rolling daily budgets. It is safe for
// concurrent use: each Wait reserves the next slot atomically under the mutex
// before sleeping, so concurrent callers of the same class cannot bunch up.
type Limiter struct {
	mu      sync.Mutex
	budgets map[Class]Budget
	// nextAt is the earliest time the next action of a class may run. Reserving
	// it under the lock serializes callers and preserves the inter-action gap.
	nextAt map[Class]time.Time
	// recent holds timestamps of actions within the rolling 24h window, used to
	// enforce DailyMax.
	recent map[Class][]time.Time
	rnd    *rand.Rand
	now    func() time.Time                           // injectable for tests
	sleep  func(context.Context, time.Duration) error // injectable for tests
}

// New returns a Limiter with the given budgets.
func New(budgets map[Class]Budget) *Limiter {
	return &Limiter{
		budgets: budgets,
		nextAt:  map[Class]time.Time{},
		recent:  map[Class][]time.Time{},
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
		now:     time.Now,
		sleep:   sleepCtx,
	}
}

// Wait blocks until it is acceptable to perform an action of the given class.
// It reserves the slot under the lock (so concurrent callers queue rather than
// race), then sleeps outside the lock. It returns ErrDailyBudget if the class's
// rolling daily budget is spent, or the context error if ctx is cancelled.
func (l *Limiter) Wait(ctx context.Context, c Class) error {
	l.mu.Lock()
	b, ok := l.budgets[c]
	if !ok {
		l.mu.Unlock()
		return nil
	}

	now := l.now()
	if b.DailyMax > 0 {
		l.pruneLocked(c, now)
		if len(l.recent[c]) >= b.DailyMax {
			l.mu.Unlock()
			return ErrDailyBudget
		}
	}

	// Reserve a slot at or after the class's earliest allowed time.
	gap := b.MinGap
	if b.MaxGap > b.MinGap {
		gap += time.Duration(l.rnd.Int63n(int64(b.MaxGap - b.MinGap)))
	}
	start := now
	if na := l.nextAt[c]; na.After(start) {
		start = na
	}
	l.nextAt[c] = start.Add(gap)
	l.recent[c] = append(l.recent[c], start)
	l.mu.Unlock()

	if d := start.Sub(now); d > 0 {
		return l.sleep(ctx, d)
	}
	return nil
}

// pruneLocked drops recorded actions older than 24h. Caller holds l.mu.
func (l *Limiter) pruneLocked(c Class, now time.Time) {
	cutoff := now.Add(-24 * time.Hour)
	ts := l.recent[c]
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		l.recent[c] = ts[i:]
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

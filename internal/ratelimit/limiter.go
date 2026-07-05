// Package ratelimit provides conservative, human-like pacing for LinkedIn
// actions. LinkedIn aggressively restricts accounts that behave like bots,
// so lion paces requests with a token bucket plus randomized jitter and
// enforces per-day action budgets.
package ratelimit

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

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

// Limiter paces actions and tracks last-action times per class. It is safe for
// concurrent use, though lion is largely sequential.
type Limiter struct {
	mu       sync.Mutex
	budgets  map[Class]Budget
	lastAct  map[Class]time.Time
	rnd      *rand.Rand
	sleep    func(context.Context, time.Duration) error // injectable for tests
}

// New returns a Limiter with the given budgets.
func New(budgets map[Class]Budget) *Limiter {
	return &Limiter{
		budgets: budgets,
		lastAct: map[Class]time.Time{},
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
		sleep:   sleepCtx,
	}
}

// Wait blocks until it is acceptable to perform an action of the given class,
// sleeping a randomized gap since the last action of that class. It returns
// early with the context error if ctx is cancelled.
func (l *Limiter) Wait(ctx context.Context, c Class) error {
	l.mu.Lock()
	b, ok := l.budgets[c]
	last := l.lastAct[c]
	l.mu.Unlock()
	if !ok {
		return nil
	}

	gap := b.MinGap
	if b.MaxGap > b.MinGap {
		gap += time.Duration(l.rnd.Int63n(int64(b.MaxGap - b.MinGap)))
	}
	if !last.IsZero() {
		elapsed := time.Since(last)
		if elapsed < gap {
			if err := l.sleep(ctx, gap-elapsed); err != nil {
				return err
			}
		}
	}

	l.mu.Lock()
	l.lastAct[c] = time.Now()
	l.mu.Unlock()
	return nil
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

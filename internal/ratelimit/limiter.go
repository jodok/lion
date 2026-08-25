// Package ratelimit provides conservative, human-like pacing for LinkedIn
// actions. LinkedIn aggressively restricts accounts that behave like bots,
// so lion paces requests with randomized inter-action gaps and enforces
// per-day action budgets.
package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jodok/lion/internal/config"
)

// ErrDailyBudget is returned when a class's daily action budget is spent.
var ErrDailyBudget = errors.New("daily action budget exhausted for this class")

// Class groups actions by how sensitive LinkedIn is to them. Reads are cheap;
// invites are the most likely to trigger restrictions.
type Class int

const (
	Read Class = iota
	Write
	Invite
)

// name returns the stable identifier used for this class in the persisted
// state file. Unlike String()-style debug output, this must never change for
// a given Class value once shipped, since it's a JSON key on disk.
func (c Class) name() string {
	switch c {
	case Read:
		return "read"
	case Write:
		return "write"
	case Invite:
		return "invite"
	default:
		return fmt.Sprintf("class%d", int(c))
	}
}

// classByName reverses Class.name, for loading persisted state.
func classByName(name string) (Class, bool) {
	switch name {
	case "read":
		return Read, true
	case "write":
		return Write, true
	case "invite":
		return Invite, true
	default:
		return 0, false
	}
}

// Budget describes pacing for one action class.
type Budget struct {
	// MinGap and MaxGap bound the randomized delay between actions.
	MinGap, MaxGap time.Duration
	// DailyMax caps actions per calendar day (0 = unlimited). The day is
	// determined by the limiter's clock (real wall-clock time in
	// production); it resets at local midnight, not on a rolling 24h basis.
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

// stateFileName is the JSON file lion persists limiter state to under the
// lion home directory, so daily budgets and inter-action pacing survive
// across process invocations. Without this, a shell loop spawning one
// `lion invite` per target would bypass both the daily cap and the jitter
// entirely: each invocation would start a fresh, empty in-memory limiter.
const stateFileName = "ratelimit.json"

// persistedState is the on-disk shape. Date is compared as a plain string
// (YYYY-MM-DD) so a calendar-day rollover is a simple inequality check.
type persistedState struct {
	Date   string               `json:"date"`
	Counts map[string]int       `json:"counts"`
	NextAt map[string]time.Time `json:"next_at"`
}

// DefaultStatePath resolves the persisted limiter state file path under the
// lion home directory (see internal/config.EnsureHome), creating the home
// directory if needed.
func DefaultStatePath() (string, error) {
	home, err := config.EnsureHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, stateFileName), nil
}

// Limiter paces actions and enforces daily budgets. It is safe for
// concurrent use: each Wait reserves the next slot atomically under the
// mutex before sleeping, so concurrent callers of the same class cannot
// bunch up.
type Limiter struct {
	mu      sync.Mutex
	budgets map[Class]Budget
	// nextAt is the earliest time the next action of a class may run.
	// Reserving it under the lock serializes callers and preserves the
	// inter-action gap. A class with no entry has no known history (neither
	// in this process nor loaded from a state file).
	nextAt map[Class]time.Time
	// counts holds how many actions of each class have run on `date`.
	counts map[Class]int
	// date is the calendar day (YYYY-MM-DD) counts is for; it resets counts
	// to zero the first time Wait observes a different day.
	date string
	// statePath is where state is persisted; "" disables persistence.
	statePath string
	rnd       *rand.Rand
	now       func() time.Time                           // injectable for tests
	sleep     func(context.Context, time.Duration) error // injectable for tests
}

// New returns an in-memory Limiter with the given budgets. State is not
// persisted across process invocations; use NewPersistent or NewDefault for
// production use so budgets and pacing survive a fresh `lion` invocation.
func New(budgets map[Class]Budget) *Limiter {
	return &Limiter{
		budgets: budgets,
		nextAt:  map[Class]time.Time{},
		counts:  map[Class]int{},
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
		now:     time.Now,
		sleep:   sleepCtx,
	}
}

// NewPersistent returns a Limiter backed by a JSON state file at statePath,
// so daily budgets and the last-action timestamp survive across process
// invocations — a shell loop spawning one `lion invite` per target still
// hits the same daily cap and jitter a single long-running process would.
// Any existing state at statePath is loaded immediately; a missing or
// corrupt file is treated as "no prior state" rather than an error, since
// persistence is a best-effort safety net, not a source of truth lion must
// have to function.
func NewPersistent(budgets map[Class]Budget, statePath string) *Limiter {
	l := New(budgets)
	l.statePath = statePath
	l.load()
	return l
}

// NewDefault returns a Limiter backed by the default persisted state file
// under the lion home directory (internal/config.EnsureHome). If that
// directory can't be resolved or created, it falls back to an in-memory
// (non-persisted) limiter — a construction-time filesystem hiccup should
// degrade pacing durability, not stop lion from working at all.
func NewDefault(budgets map[Class]Budget) *Limiter {
	path, err := DefaultStatePath()
	if err != nil {
		return New(budgets)
	}
	return NewPersistent(budgets, path)
}

// Wait blocks until it is acceptable to perform an action of the given
// class. It reserves the slot under the lock (so concurrent callers queue
// rather than race), then sleeps outside the lock. It returns
// ErrDailyBudget if the class's daily budget is spent, or the context error
// if ctx is cancelled.
func (l *Limiter) Wait(ctx context.Context, c Class) error {
	l.mu.Lock()
	b, ok := l.budgets[c]
	if !ok {
		l.mu.Unlock()
		return nil
	}

	var now, start time.Time
	var budgetErr error

	reserve := func() {
		now = l.now()
		l.resetIfNewDayLocked(now)

		if b.DailyMax > 0 && l.counts[c] >= b.DailyMax {
			budgetErr = ErrDailyBudget
			return
		}

		gap := b.MinGap
		if b.MaxGap > b.MinGap {
			gap += time.Duration(l.rnd.Int63n(int64(b.MaxGap - b.MinGap)))
		}

		// Reserve a slot at or after the class's earliest allowed time. A
		// class with no recorded nextAt — never run in this process, and
		// nothing persisted from a prior one — still incurs the full jittered
		// gap: a freshly started process must not fire its very first mutating
		// action instantly just because there's no history yet to pace against.
		start = now
		if na, seen := l.nextAt[c]; seen {
			if na.After(start) {
				start = na
			}
		} else {
			start = now.Add(gap)
		}
		l.nextAt[c] = start.Add(gap)
		l.counts[c]++
		l.saveLocked()
	}

	// Reserving is a read-modify-write of state shared across processes: the
	// mutex above only orders goroutines, but every `lion` invocation is its
	// own process. Without an inter-process lock two concurrent runs both read
	// the same counts, both reserve, and the later write erases the earlier
	// reservation — reinstating exactly the shell-loop bypass that persisting
	// the budget exists to prevent. Re-read inside the lock so this run paces
	// against what other processes have already committed.
	if l.statePath == "" {
		reserve()
	} else {
		withStateLock(l.statePath, func() {
			l.load()
			reserve()
		})
	}
	l.mu.Unlock()

	if budgetErr != nil {
		return budgetErr
	}
	if d := start.Sub(now); d > 0 {
		return l.sleep(ctx, d)
	}
	return nil
}

// withStateLock runs fn while holding a best-effort inter-process lock for the
// limiter's state file. Like the credential store's lock it is advisory and
// time-bounded: after maxWait it proceeds anyway rather than hanging a command,
// and it only removes a lock file this call actually created — deleting one
// after giving up would break the mutex for the process still holding it.
func withStateLock(statePath string, fn func()) {
	const (
		retryDelay   = 25 * time.Millisecond
		maxWait      = 5 * time.Second
		staleLockAge = 30 * time.Second
	)
	lp := statePath + ".lock"
	deadline := time.Now().Add(maxWait)
	acquired := false
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			acquired = true
			break
		}
		if !errors.Is(err, os.ErrExist) {
			break // cannot lock at all (unwritable dir): pace rather than fail
		}
		if fi, statErr := os.Stat(lp); statErr == nil && time.Since(fi.ModTime()) > staleLockAge {
			os.Remove(lp) // steal a stale lock left by a crashed process
		}
		// Check the deadline even after stealing, so a lock that keeps being
		// recreated cannot spin here past maxWait.
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(retryDelay)
	}
	if acquired {
		defer os.Remove(lp)
	}
	fn()
}

// resetIfNewDayLocked zeroes the daily counters when now falls on a
// different calendar day than the one counts was last reset for. Caller
// holds l.mu.
func (l *Limiter) resetIfNewDayLocked(now time.Time) {
	today := now.Format("2006-01-02")
	if l.date == today {
		return
	}
	l.date = today
	for c := range l.counts {
		l.counts[c] = 0
	}
}

// load reads persisted state from statePath, if set, populating date,
// counts, and nextAt. Any error (missing file, corrupt JSON, unknown class
// name) is ignored: the limiter simply starts with no known history, the
// same as a fresh in-memory Limiter.
func (l *Limiter) load() {
	if l.statePath == "" {
		return
	}
	b, err := os.ReadFile(l.statePath)
	if err != nil {
		return
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return
	}
	l.date = st.Date
	for name, n := range st.Counts {
		if c, ok := classByName(name); ok {
			l.counts[c] = n
		}
	}
	for name, t := range st.NextAt {
		if c, ok := classByName(name); ok {
			l.nextAt[c] = t
		}
	}
}

// saveLocked persists the limiter's state to statePath, if set. It writes
// to a unique temp file in the same directory and renames over the final
// path, so a crash or a concurrent reader never observes a partial write;
// the file is created 0600 since it's a lightweight local action log.
// Caller holds l.mu. Errors are ignored: persistence is best-effort and
// must never block a Voyager call from proceeding.
func (l *Limiter) saveLocked() {
	if l.statePath == "" {
		return
	}
	st := persistedState{
		Date:   l.date,
		Counts: make(map[string]int, len(l.counts)),
		NextAt: make(map[string]time.Time, len(l.nextAt)),
	}
	for c, n := range l.counts {
		st.Counts[c.name()] = n
	}
	for c, t := range l.nextAt {
		st.NextAt[c.name()] = t
	}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	dir := filepath.Dir(l.statePath)
	tmp, err := os.CreateTemp(dir, ".ratelimit-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpPath)
		return
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, l.statePath); err != nil {
		os.Remove(tmpPath)
		return
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

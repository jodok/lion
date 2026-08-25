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

// ErrBudgetLock is returned when the inter-process state lock could not be
// acquired. The limiter fails closed rather than reserving unlocked: two
// processes reserving against the same state file without serialization is
// exactly the cross-process double-spend persistence exists to prevent, so
// an unavailable lock must abort the reservation, not fall back to it.
var ErrBudgetLock = errors.New("ratelimit: could not acquire inter-process state lock")

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
	// DailyMax caps actions per rolling 24-hour window (0 = unlimited),
	// counted back from the limiter's clock (real wall-clock time in
	// production). It is intentionally not a calendar-day counter: a
	// calendar-day reset lets a caller spend the whole budget right before
	// local midnight and the whole budget again right after, doubling the
	// intended burst.
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

// budgetWindow is the rolling window DailyMax is measured over. It is a
// named constant, not folded into the arithmetic at each use, so the
// "24 hours" in the budget check, the load-time prune, and the save-time
// prune are provably the same window rather than three numbers that could
// drift apart under future edits.
const budgetWindow = 24 * time.Hour

// persistedState is the on-disk shape. Actions holds, per class name, the
// timestamps of recent actions of that class; a rolling-window budget check
// is "how many of these are newer than now - budgetWindow", so the raw
// timestamps — not a precomputed count — are what must survive a process
// restart.
type persistedState struct {
	Actions map[string][]time.Time `json:"actions"`
	NextAt  map[string]time.Time   `json:"next_at"`
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
	// actions holds, per class, the timestamps of recent actions still
	// inside budgetWindow. The DailyMax check counts entries here rather
	// than a running counter, so the budget is a true rolling window instead
	// of one that can be doubled by acting just before and after a
	// calendar-day boundary.
	actions map[Class][]time.Time
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
		actions: map[Class][]time.Time{},
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
// On a platform with no inter-process lock it also falls back to in-memory:
// persisted state that cannot be serialized between processes would make Wait
// fail closed on every call, leaving the client unable to issue any request at
// all. Weaker pacing (per-process budgets, as before persistence existed) is
// the lesser evil versus a CLI that cannot run; lion ships unix binaries, where
// the persisted path is always the one taken.
func NewDefault(budgets map[Class]Budget) *Limiter {
	if !lockSupported {
		return New(budgets)
	}
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
		l.pruneLocked(now)

		if b.DailyMax > 0 && len(l.actions[c]) >= b.DailyMax {
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
		l.actions[c] = append(l.actions[c], now)
		l.saveLocked()
	}

	// Reserving is a read-modify-write of state shared across processes: the
	// mutex above only orders goroutines, but every `lion` invocation is its
	// own process. Without an inter-process lock two concurrent runs both read
	// the same actions, both reserve, and the later write erases the earlier
	// reservation — reinstating exactly the shell-loop bypass that persisting
	// the budget exists to prevent. Re-read inside the lock so this run paces
	// against what other processes have already committed.
	//
	// The lock is flock-based and blocking rather than a time-bounded
	// heuristic: flock is released by the kernel the instant the holding
	// process exits, crashes, or is killed, so there is no stale-lock file to
	// misjudge and therefore no case where proceeding unlocked would be safe.
	// If the lock genuinely cannot be acquired (e.g. an unwritable directory),
	// this run fails closed instead of reserving against possibly-stale state.
	if l.statePath == "" {
		reserve()
	} else {
		unlock, err := lockFile(l.statePath + ".lock")
		if err != nil {
			l.mu.Unlock()
			return fmt.Errorf("%w: %v", ErrBudgetLock, err)
		}
		l.load()
		reserve()
		unlock()
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

// pruneLocked drops action timestamps older than budgetWindow, so neither
// the in-memory maps nor the persisted state file grow without bound across
// a long-lived process or many `lion` invocations over time. Caller holds
// l.mu.
func (l *Limiter) pruneLocked(now time.Time) {
	cutoff := now.Add(-budgetWindow)
	for c, ts := range l.actions {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.actions, c)
		} else {
			l.actions[c] = kept
		}
	}
}

// load reads persisted state from statePath, if set, populating actions and
// nextAt. Any error (missing file, corrupt JSON, unknown class name) is
// ignored: the limiter simply starts with no known history, the same as a
// fresh in-memory Limiter. Timestamps are pruned immediately after loading
// so a state file that predates a long idle period doesn't resurrect
// long-expired actions into the rolling window.
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
	for name, ts := range st.Actions {
		if c, ok := classByName(name); ok {
			l.actions[c] = append([]time.Time(nil), ts...)
		}
	}
	for name, t := range st.NextAt {
		if c, ok := classByName(name); ok {
			l.nextAt[c] = t
		}
	}
	l.pruneLocked(l.now())
}

// saveLocked persists the limiter's state to statePath, if set. It writes
// to a unique temp file in the same directory and renames over the final
// path, so a crash or a concurrent reader never observes a partial write;
// the file is created 0600 since it's a lightweight local action log.
// Caller holds l.mu. Errors are ignored: persistence is best-effort and
// must never block a Voyager call from proceeding. Pruning again here (load
// already pruned) keeps the file from growing unboundedly for a long-lived
// process that never restarts and so never re-runs load's prune.
func (l *Limiter) saveLocked() {
	if l.statePath == "" {
		return
	}
	l.pruneLocked(l.now())
	st := persistedState{
		Actions: make(map[string][]time.Time, len(l.actions)),
		NextAt:  make(map[string]time.Time, len(l.nextAt)),
	}
	for c, ts := range l.actions {
		st.Actions[c.name()] = ts
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

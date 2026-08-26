package lockfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireIsExclusive verifies flock actually blocks a second acquirer
// until the first releases — the property internal/ratelimit's Wait depends
// on for its blocking critical section.
func TestAcquireIsExclusive(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	path := filepath.Join(t.TempDir(), "test.lock")

	release1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		release2, err := Acquire(path)
		if err != nil {
			t.Errorf("second Acquire: %v", err)
			return
		}
		close(acquired)
		release2()
	}()

	select {
	case <-acquired:
		t.Fatal("second Acquire succeeded while the first still held the lock")
	case <-time.After(200 * time.Millisecond):
		// Expected: the second acquirer is still blocked.
	}

	release1()

	select {
	case <-acquired:
		// Expected: releasing the first lock let the second proceed.
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never succeeded after the first released")
	}
}

// TestTryAcquireReturnsErrLocked covers the non-blocking path sync's store
// lock relies on: a second TryAcquire must fail immediately with ErrLocked,
// not block, while the first holder is still alive.
func TestTryAcquireReturnsErrLocked(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	path := filepath.Join(t.TempDir(), "test.lock")

	lock, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	defer lock.Release()

	if _, err := TryAcquire(path); err != ErrLocked {
		t.Fatalf("second TryAcquire = %v, want ErrLocked", err)
	}
}

// TestTryAcquireSucceedsAfterRelease pins that releasing actually frees the
// lock for a subsequent TryAcquire, rather than merely closing this
// process's handle to it.
func TestTryAcquireSucceedsAfterRelease(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	path := filepath.Join(t.TempDir(), "test.lock")

	lock, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	lock.Release()

	lock2, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after release: %v", err)
	}
	lock2.Release()
}

// TestWriteInfoReadInfoRoundTrips covers the diagnostic info channel `lion
// sync` uses to report who's holding the lock, through the real
// TryAcquire/WriteInfo/ReadInfo path (rather than exercising a standalone
// path-based WriteInfo helper, which no longer exists — see
// TestWriteInfoWritesThroughDescriptorNotByName for why).
func TestWriteInfoReadInfoRoundTrips(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	path := filepath.Join(t.TempDir(), "test.lock")

	lock, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer lock.Release()

	if err := lock.WriteInfo([]byte("pid=1234")); err != nil {
		t.Fatal(err)
	}
	if got := string(ReadInfo(path)); got != "pid=1234" {
		t.Errorf("ReadInfo = %q, want %q", got, "pid=1234")
	}

	// A shorter second write must not leave a trailing remnant of the first
	// — WriteInfo truncates through the descriptor before writing.
	if err := lock.WriteInfo([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := string(ReadInfo(path)); got != "x" {
		t.Errorf("ReadInfo after shorter WriteInfo = %q, want %q", got, "x")
	}
}

// TestReadInfoMissingFileReturnsNil pins the "unknown holder" fallback: a
// missing file must not be an error a caller has to handle separately.
func TestReadInfoMissingFileReturnsNil(t *testing.T) {
	if got := ReadInfo(filepath.Join(t.TempDir(), "does-not-exist.lock")); got != nil {
		t.Errorf("ReadInfo(missing) = %q, want nil", got)
	}
}

// TestTryAcquireRejectsSymlink pins the core of finding #3: a scheduled
// `lion sync` whose --store lives in a shared directory can be attacked by
// pre-creating <store>.lock as a symlink to a file the syncing user can
// write. TryAcquire must refuse to lock (and, by extension, WriteInfo must
// never get a chance to overwrite) a path that is a symlink, and the
// symlink's target must come out untouched.
func TestTryAcquireRejectsSymlink(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	const victimContent = "not a lock file"
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(dir, "store.lock")
	if err := os.Symlink(victim, lockPath); err != nil {
		t.Fatal(err)
	}

	if _, err := TryAcquire(lockPath); err == nil {
		t.Fatal("TryAcquire on a symlinked lock path succeeded, want a refusal")
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != victimContent {
		t.Fatalf("victim file was modified: got %q, want %q", got, victimContent)
	}
}

// TestAcquireRejectsSymlink is TestTryAcquireRejectsSymlink's counterpart
// for the blocking Acquire path (internal/ratelimit's persisted-state
// lock), which shares the same openLockFile guard.
func TestAcquireRejectsSymlink(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	const victimContent = "not a lock file"
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(dir, "state.lock")
	if err := os.Symlink(victim, lockPath); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(lockPath); err == nil {
		t.Fatal("Acquire on a symlinked lock path succeeded, want a refusal")
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != victimContent {
		t.Fatalf("victim file was modified: got %q, want %q", got, victimContent)
	}
}

// TestWriteInfoWritesThroughDescriptorNotByName pins the second half of
// finding #3: even if something replaces the lock path with a symlink to a
// victim file *after* TryAcquire already holds the lock, WriteInfo must
// still write to the file it locked — because it writes through the
// descriptor opened at acquire time — and must never touch whatever the
// path now resolves to. This is the regression a path-based
// re-open-and-write (the old lockfile.WriteInfo(path, b)) could not pass.
func TestWriteInfoWritesThroughDescriptorNotByName(t *testing.T) {
	if !Supported() {
		t.Skip("no inter-process lock on this platform")
	}
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "store.lock")

	lock, err := TryAcquire(lockPath)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer lock.Release()

	// Simulate the attack window: something with write access to dir swaps
	// the lock path out for a symlink to a file the lock holder shouldn't
	// touch, after the lock was already taken.
	victim := filepath.Join(dir, "victim.txt")
	const victimContent = "not a lock file"
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, lockPath); err != nil {
		t.Fatal(err)
	}

	if err := lock.WriteInfo([]byte("pid=1234")); err != nil {
		t.Fatalf("WriteInfo: %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != victimContent {
		t.Fatalf("victim file was modified by WriteInfo: got %q, want %q", got, victimContent)
	}
}

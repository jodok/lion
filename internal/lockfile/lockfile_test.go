package lockfile

import (
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

	release, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	defer release()

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

	release, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	release()

	release2, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after release: %v", err)
	}
	release2()
}

// TestWriteInfoReadInfoRoundTrips covers the diagnostic info channel `lion
// sync` uses to report who's holding the lock.
func TestWriteInfoReadInfoRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	if err := WriteInfo(path, []byte("pid=1234")); err != nil {
		t.Fatal(err)
	}
	if got := string(ReadInfo(path)); got != "pid=1234" {
		t.Errorf("ReadInfo = %q, want %q", got, "pid=1234")
	}
}

// TestReadInfoMissingFileReturnsNil pins the "unknown holder" fallback: a
// missing file must not be an error a caller has to handle separately.
func TestReadInfoMissingFileReturnsNil(t *testing.T) {
	if got := ReadInfo(filepath.Join(t.TempDir(), "does-not-exist.lock")); got != nil {
		t.Errorf("ReadInfo(missing) = %q, want nil", got)
	}
}

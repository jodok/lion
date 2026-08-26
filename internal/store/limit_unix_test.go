//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// oversizedTxAgainstCap runs the same shape of transaction the existing
// hard-bound test does (TestSetMaxSizeRejectsAndRollsBackOversizedTransaction
// in limit_test.go): a SetMaxSize ceiling, then an insert that overflows it.
// It's factored out so the two tests below can each install a different
// diskFree before triggering the same SQLITE_FULL.
//
// The cap is 300 pages, not the 20 pages limit_test.go's own version uses:
// SizeBytes (which capHeadroom is computed from) counts the WAL/shm
// sidecars, and a freshly-migrated store's un-checkpointed WAL already
// occupies well more than 20 pages' worth of bytes on its own — 20 pages
// would make capHeadroom negative before the transaction even runs, which
// would make every SQLITE_FULL here look cap-bound regardless of diskFree
// and defeat the point of these two tests. 300 pages leaves capHeadroom
// comfortably positive going in while still being far below what the bulk
// insert below writes, so the insert still overflows it.
func oversizedTxAgainstCap(t *testing.T, s *Store) error {
	t.Helper()
	var pageSize int64
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMaxSize(300 * pageSize); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	longBody := strings.Repeat("x", 20000)
	return s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.UpsertConversation(ctx, Conversation{ID: "c1", URN: "urn:li:fs_conversation:c1", UpdatedAt: 1}, 1); err != nil {
			return err
		}
		msgs := make([]Message, 50)
		for i := range msgs {
			msgs[i] = Message{URN: fmt.Sprintf("m%d", i), ConversationID: "c1", SentAt: int64(i), Body: longBody}
		}
		_, err := tx.RecordMessagePage(ctx, "c1", msgs, 1)
		return err
	})
}

// TestAsDatabaseFullAttributesCapWhenDiskHasHeadroom is the requested
// cap-binding case: a small --max-db-size ceiling, hit while the filesystem
// itself has plenty of room, must be reported as ErrDatabaseFull — the cap,
// not the disk, is what stopped the write. diskFree is overridden (rather
// than relying on t.TempDir()'s real free space) so the test is deterministic
// and independent of how full the machine it runs on happens to be.
func TestAsDatabaseFullAttributesCapWhenDiskHasHeadroom(t *testing.T) {
	s := newTestStore(t)
	s.diskFree = func(string) (int64, bool) { return 1 << 40, true } // 1 TiB, far more than any capHeadroom here

	err := oversizedTxAgainstCap(t, s)
	if !errors.Is(err, ErrDatabaseFull) {
		t.Fatalf("err = %v, want ErrDatabaseFull (disk reports ample free space, so the cap is what bound)", err)
	}
}

// TestAsDatabaseFullPreservesRawErrorWhenDiskIsShort is the discriminating
// regression for the fix: the old diskHasHeadroom probe wrote a trivial
// 4KiB temp file next to the store and treated that write succeeding as
// proof the disk wasn't full. That reasoning breaks the moment the disk has
// a few KiB free — enough for the probe, not enough for the actual failed
// transaction — which is exactly the case a real ENOSPC under a configured
// --max-db-size would hit: the probe would succeed and the genuine
// storage failure would be reported as a clean size-cap truncation instead.
//
// Standing in for "a few KiB free": diskFree overridden to report almost
// nothing, far less than capHeadroom (maxBytes - SizeBytes()) even though
// the real filesystem under t.TempDir() has plenty. Because the old
// implementation never consulted diskFree at all (it always did its own
// probe write against the real, roomy disk), it would misreport this case
// as ErrDatabaseFull; the fix must instead preserve the raw error.
func TestAsDatabaseFullPreservesRawErrorWhenDiskIsShort(t *testing.T) {
	s := newTestStore(t)
	s.diskFree = func(string) (int64, bool) { return 1, true } // effectively no room

	err := oversizedTxAgainstCap(t, s)
	if err == nil {
		t.Fatal("expected the write to fail against the tiny page ceiling")
	}
	if errors.Is(err, ErrDatabaseFull) {
		t.Errorf("a SQLITE_FULL was attributed to the --max-db-size cap despite diskFree reporting almost no room: %v", err)
	}
}

// TestDiskFreeBytesReportsRealFreeSpace is a light sanity check that the
// unix diskFreeBytes implementation actually talks to statfs(2) and returns
// a plausible answer, rather than only ever being exercised through the
// injected s.diskFree override above.
func TestDiskFreeBytesReportsRealFreeSpace(t *testing.T) {
	free, ok := diskFreeBytes(t.TempDir())
	if !ok {
		t.Fatal("diskFreeBytes: ok = false, want true on a real directory")
	}
	if free <= 0 {
		t.Errorf("diskFreeBytes = %d, want > 0", free)
	}
}

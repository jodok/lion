package store

import "os"

// SizeBytes returns the store's total on-disk footprint: the main database
// file plus its WAL and shared-memory sidecar files (present under
// PRAGMA journal_mode=WAL, see Open). `lion sync --max-db-size` checks this
// before committing each page, and the WAL file can hold a meaningful
// fraction of recent writes between checkpoints, so counting only the main
// file would under-report right when the limit matters most.
func (s *Store) SizeBytes() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(s.path + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}

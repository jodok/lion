package cli

import (
	"strconv"
	"strings"
	"time"
)

// parseTimeFlag parses a --after/--before value, accepting either RFC3339
// (for precision) or a bare YYYY-MM-DD date (for convenience), matching
// wacli's own --after/--before grammar. A date-only value is interpreted as
// UTC midnight — the store's own timestamps (LinkedIn's, via the Voyager
// API) are all UTC epoch-ms, so anchoring the date boundary to UTC keeps
// "--after 2026-01-01" meaning the same instant regardless of the caller's
// local timezone.
func parseTimeFlag(name, s string) (int64, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().UnixMilli(), nil
	}
	return 0, usageErr("--%s: %q is not RFC3339 (e.g. 2026-01-02T15:04:05Z) or YYYY-MM-DD", name, s)
}

// sizeMultipliers maps a case-insensitive suffix to its byte multiplier,
// checked longest-suffix-first so "MB" doesn't get caught by a bare "B"
// rule before it has a chance to match.
var sizeMultipliers = []struct {
	suffix string
	mult   int64
}{
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"KB", 1 << 10},
	{"B", 1},
}

// parseSize parses a human-readable byte size for --max-db-size (e.g.
// "500MB", "2GB"), or a bare integer as a byte count. Multipliers are
// binary (1MB = 1<<20 bytes) since that's what matches a filesystem's own
// idea of file size, which is what --max-db-size is actually bounding.
func parseSize(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, usageErr("--max-db-size: empty value")
	}
	upper := strings.ToUpper(trimmed)
	for _, m := range sizeMultipliers {
		if !strings.HasSuffix(upper, m.suffix) {
			continue
		}
		numPart := strings.TrimSpace(strings.TrimSuffix(upper, m.suffix))
		n, err := strconv.ParseFloat(numPart, 64)
		if err != nil || n < 0 {
			return 0, usageErr("--max-db-size: %q is not a valid size (want e.g. 500MB, 2GB, or a bare byte count)", s)
		}
		return int64(n * float64(m.mult)), nil
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || n < 0 {
		return 0, usageErr("--max-db-size: %q is not a valid size (want e.g. 500MB, 2GB, or a bare byte count)", s)
	}
	return n, nil
}

// formatDuration renders a duration the way sync's human progress line
// wants it: whole seconds for anything under a minute, otherwise Go's own
// compact form (e.g. "1h2m3s") — precise enough to be useful without the
// sub-second noise time.Duration.String() adds for a long-running command.
func formatDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}

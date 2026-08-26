package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// progressReporter renders `lion sync`'s progress to stderr — never stdout,
// which is data-only (DESIGN.md §2.3) — in one of three mutually exclusive
// modes, matching wacli's own convention for long-running commands:
//
//   - live: a single status line that redraws in place, used when stderr is
//     an interactive terminal. This is what a person watching a sync
//     actually wants — a scrollback that isn't one line per message.
//   - plain: one line per status update, appended rather than redrawn, used
//     when stderr is redirected to a file or pipe (a redrawing line would
//     just be a wall of \r-separated garbage in a log).
//   - events (--events): NDJSON lifecycle events instead of either, for a
//     caller that wants to parse progress programmatically.
//
// Warnings always print on their own line in both live and plain mode —
// never folded into the status line — so a live redraw never erases one.
type progressReporter struct {
	w      io.Writer
	events bool
	live   bool
	// wroteLine tracks whether the live status line currently has content
	// sitting on it, so Warn knows whether it needs to move to a fresh line
	// first, and Close knows whether it needs a trailing newline.
	wroteLine bool
}

// newProgressReporter builds a progressReporter writing to f. Live redraw
// mode is only used when events is false and f is an interactive terminal —
// reusing isTerminal (see root.go) rather than introducing a second way to
// detect one, per the correction that prompted this: root.go's TTY check
// is already the source of truth lion uses for "is anyone watching this".
func newProgressReporter(f *os.File, events bool) *progressReporter {
	return &progressReporter{w: f, events: events, live: !events && isTerminal(f)}
}

// Status reports a one-line progress update. In live mode it redraws the
// current line in place; in plain mode it appends a new line; under
// --events it is a no-op (use Event instead).
func (p *progressReporter) Status(format string, a ...any) {
	if p.events {
		return
	}
	msg := fmt.Sprintf(format, a...)
	if p.live {
		// \r returns to column 0; \x1b[K clears from the cursor to the end
		// of the line, erasing any leftover tail from a longer previous
		// status without needing to pad msg to a fixed width.
		fmt.Fprintf(p.w, "\r%s\x1b[K", msg)
	} else {
		fmt.Fprintln(p.w, msg)
	}
	p.wroteLine = true
}

// Warn reports a warning that must survive on its own line rather than be
// overwritten by the next Status redraw. It always prints, even under
// --events (as an "event":"warning" line), since a warning is exactly the
// kind of thing a caller parsing NDJSON progress still needs to see.
func (p *progressReporter) Warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if p.events {
		p.Event("warning", map[string]any{"message": msg})
		return
	}
	if p.live && p.wroteLine {
		// Move off the in-progress status line before printing, so the
		// warning doesn't get clobbered by \r on the next Status call and
		// isn't itself overwritten.
		fmt.Fprintln(p.w)
	}
	fmt.Fprintln(p.w, "warning:", msg)
	p.wroteLine = false
}

// Event writes one NDJSON lifecycle event ({"event":name, "ts":..., ...
// fields}) to stderr. Only meaningful when --events is set; Status/Warn
// call this internally for the events they need to surface either way.
func (p *progressReporter) Event(name string, fields map[string]any) {
	if !p.events {
		return
	}
	evt := map[string]any{"event": name, "ts": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range fields {
		evt[k] = v
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return // a progress event is best-effort; never fail the sync over it
	}
	fmt.Fprintln(p.w, string(b))
}

// Close finishes the live status line (if one is on-screen) with a trailing
// newline, so whatever prints next — a warning, the final summary, the
// shell prompt — doesn't land mid-line.
func (p *progressReporter) Close() {
	if p.live && p.wroteLine {
		fmt.Fprintln(p.w)
	}
}

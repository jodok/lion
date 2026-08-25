// Package output renders command results in one of several formats, following
// the gogcli contract: stdout carries data only; prompts, progress, and
// warnings go to stderr.
package output

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Format selects how results are rendered.
type Format int

const (
	// FormatTable is the default human-readable, column-aligned format.
	FormatTable Format = iota
	// FormatJSON emits a single JSON value.
	FormatJSON
	// FormatPlain emits tab-separated values (one record per line).
	FormatPlain
)

// Renderer writes results to an output stream in the chosen format.
type Renderer struct {
	w             io.Writer
	format        Format
	wrapUntrusted bool
}

// New returns a Renderer. wrapUntrusted, when true, wraps free-text values
// captured from LinkedIn in delimiters so downstream LLMs treat them as data.
func New(w io.Writer, f Format, wrapUntrusted bool) *Renderer {
	return &Renderer{w: w, format: f, wrapUntrusted: wrapUntrusted}
}

// Table describes tabular data: Cols are headers, Rows are string cells.
type Table struct {
	Cols []string
	Rows [][]string
}

// Emit renders v. For JSON it marshals v directly. For Table/Plain it expects
// v to be a *Table (callers build the table); anything else is JSON-encoded as
// a fallback so no command can silently emit nothing.
func (r *Renderer) Emit(v any) error {
	switch r.format {
	case FormatJSON:
		enc := json.NewEncoder(r.w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case FormatPlain:
		t, ok := v.(*Table)
		if !ok {
			return r.emitJSON(v)
		}
		return r.emitPlain(t)
	default:
		t, ok := v.(*Table)
		if !ok {
			return r.emitJSON(v)
		}
		return r.emitTable(t)
	}
}

func (r *Renderer) emitJSON(v any) error {
	enc := json.NewEncoder(r.w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (r *Renderer) emitPlain(t *Table) error {
	for _, row := range t.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = sanitizeTSV(c)
		}
		if _, err := fmt.Fprintln(r.w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return nil
}

func (r *Renderer) emitTable(t *Table) error {
	tw := tabwriter.NewWriter(r.w, 0, 2, 2, ' ', 0)
	if len(t.Cols) > 0 {
		if _, err := fmt.Fprintln(tw, strings.Join(t.Cols, "\t")); err != nil {
			return err
		}
	}
	for _, row := range t.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = collapse(c)
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// Untrusted wraps free text captured from LinkedIn when --wrap-untrusted is
// set, marking it as data rather than instructions for any downstream LLM.
//
// Format: <untrusted nonce=HEX>\n...\n</untrusted nonce=HEX>, where HEX is a
// fresh random 16-hex-digit (8-byte) token generated for this call. Consumers
// that parse the boundary must match the nonce on both the opening and
// closing tags rather than matching a fixed "</untrusted>" string.
//
// A fixed delimiter would let LinkedIn-controlled payload text simply embed
// a literal "</untrusted>" and forge the end of the wrapper (prompt-injection
// escape) — a message body containing that string would prematurely close
// the block and let anything after it be read as trusted output. Tagging the
// boundary with a per-call random nonce means the payload cannot know it in
// advance, so it cannot forge a matching terminator: the only way to close
// the block is to already know the nonce, which is generated fresh and never
// exposed to the wrapped text itself.
func (r *Renderer) Untrusted(s string) string {
	if !r.wrapUntrusted {
		return s
	}
	nonce := randomNonce()
	return "<untrusted nonce=" + nonce + ">\n" + s + "\n</untrusted nonce=" + nonce + ">"
}

// randomNonce returns a fresh 16-hex-digit random token for tagging an
// Untrusted boundary. Unpredictability is the whole mechanism: wrapped text
// cannot close a boundary whose terminator it cannot guess, so a fixed
// fallback nonce would reintroduce the exact escape this tagging prevents.
// Since Go 1.24, crypto/rand.Read never returns an error (it terminates the
// process if the OS entropy source fails), so there is no degraded path worth
// taking here.
func randomNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sanitizeTSV neutralizes tabs/newlines that would corrupt TSV rows, then
// strips the control characters a terminal acts on rather than prints.
func sanitizeTSV(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return stripTerminalControls(s)
}

// stripTerminalControls removes characters a terminal interprets instead of
// displaying. Everything rendered here is free text written by other LinkedIn
// users — message bodies, post text, display names — and it lands in the
// default table and --plain output, so without this a sender can embed
// ANSI/OSC escapes and make lion's own output lie: repaint or erase lines to
// hide what was really received, forge the look of another command, or drive
// the terminal itself (OSC 52 writes the reader's clipboard). --wrap-untrusted
// does not help here: it marks text as data for a downstream LLM, but the
// bytes still reach the terminal on the way.
//
// JSON output deliberately does not go through this: encoding/json already
// escapes control runes to \uXXXX, so they arrive inert, and stripping them
// would corrupt values a consumer needs byte-for-byte.
//
// Removed rather than escaped: the goal is that the text can no longer drive
// the terminal, and rendering a visible \x1b inside someone's name would be
// noise without adding safety.
func stripTerminalControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		// C0 controls and DEL — this is where ESC (0x1b) and BEL (0x07) live.
		case r < 0x20, r == 0x7f:
			return -1
		// C1 controls: 0x9b is a single-byte CSI, equivalent to ESC [.
		case r >= 0x80 && r <= 0x9f:
			return -1
		// Bidi overrides and isolates, which reorder what the reader sees
		// without changing the underlying bytes — the same spoofing problem
		// wearing a different coat.
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

// collapse trims a cell to a single line for table display.
func collapse(s string) string {
	return strings.TrimSpace(sanitizeTSV(s))
}

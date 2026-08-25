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

// sanitizeTSV neutralizes tabs/newlines that would corrupt TSV rows.
func sanitizeTSV(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// collapse trims a cell to a single line for table display.
func collapse(s string) string {
	return strings.TrimSpace(sanitizeTSV(s))
}

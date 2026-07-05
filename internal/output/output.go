// Package output renders command results in one of several formats, following
// the gogcli contract: stdout carries data only; prompts, progress, and
// warnings go to stderr.
package output

import (
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
func (r *Renderer) Untrusted(s string) string {
	if !r.wrapUntrusted {
		return s
	}
	return "<untrusted>\n" + s + "\n</untrusted>"
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
